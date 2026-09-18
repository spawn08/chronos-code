package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/verification"
)

type repairEvidenceProvider struct {
	recordChecks bool
	requests     []*model.ChatRequest
}

func (p *repairEvidenceProvider) Chat(ctx context.Context, request *model.ChatRequest) (*model.ChatResponse, error) {
	p.requests = append(p.requests, request)
	if p.recordChecks {
		runtime, _ := taskRuntimeFromContext(ctx)
		exitCode := 0
		now := time.Now()
		_, _ = runtime.recordCommand("go test ./...", execution.CommandTest, &exitCode, execution.TerminalExited, nil, execution.ProvenanceRuntime, now, now)
		_, _ = runtime.recordCommand("git diff --check", execution.CommandDiff, &exitCode, execution.TerminalExited, nil, execution.ProvenanceRuntime, now, now)
	}
	return &model.ChatResponse{Role: model.RoleAssistant, Content: "repaired"}, nil
}

func (p *repairEvidenceProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, nil
}

func (p *repairEvidenceProvider) Name() string  { return "repair" }
func (p *repairEvidenceProvider) Model() string { return "claude-haiku-4-5" }

func TestRepairBlockingContinuesSessionWithBoundedProjection(t *testing.T) {
	runtime, err := newTaskRuntimeWithLimits("task", t.TempDir(), execution.TaskLimits{RepairAttempts: 1, ModelCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.recordWrite("main.go", "hash", 1, execution.ProvenanceRuntime, time.Now()); err != nil {
		t.Fatal(err)
	}
	provider := &repairEvidenceProvider{recordChecks: true}
	a := newExecutionTestAgent("coder", provider)
	orch := &Orchestrator{}
	ctx := withTaskRuntime(context.Background(), runtime)
	request := ExecutionRequest{VerificationMode: verification.ModeEnforce}
	response, decision, reason, err := orch.repairBlocking(ctx, a, "", request, router.Classification{Kind: router.TaskKindEdit}, runtime, &model.ChatResponse{Content: "initial"})
	if err != nil || response.Content != "repaired" || !decision.Allowed || reason != execution.StopSuccess {
		t.Fatalf("repair = (%#v, %#v, %q, %v)", response, decision, reason, err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("repair calls = %d, want 1", len(provider.requests))
	}
	prompt := provider.requests[0].Messages[len(provider.requests[0].Messages)-1].Content
	if strings.Contains(prompt, "initial") || !strings.Contains(prompt, "Changed paths: main.go") || !strings.Contains(prompt, "Remaining limits:") {
		t.Fatalf("repair prompt = %q", prompt)
	}
}

func TestRepairBlockingStopsRepeatedFailure(t *testing.T) {
	runtime, err := newTaskRuntimeWithLimits("task", t.TempDir(), execution.TaskLimits{RepairAttempts: 3, ModelCalls: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.recordWrite("main.go", "hash", 1, execution.ProvenanceRuntime, time.Now()); err != nil {
		t.Fatal(err)
	}
	provider := &repairEvidenceProvider{}
	a := newExecutionTestAgent("coder", provider)
	_, decision, reason, err := (&Orchestrator{}).repairBlocking(withTaskRuntime(context.Background(), runtime), a, "", ExecutionRequest{VerificationMode: verification.ModeReport}, router.Classification{Kind: router.TaskKindEdit}, runtime, nil)
	if err != nil || !decision.Disagreement || reason != execution.StopRepeatedFailure || len(provider.requests) != 1 {
		t.Fatalf("repeated repair = (%#v, %q, %v), calls=%d", decision, reason, err, len(provider.requests))
	}
}
