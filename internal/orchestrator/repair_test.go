package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
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

func TestRepairBlockingAllowsRepeatedFailureInReportMode(t *testing.T) {
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
	if err != nil || !decision.Allowed || !decision.Disagreement || reason != execution.StopSuccess || len(provider.requests) != 1 {
		t.Fatalf("repeated repair = (%#v, %q, %v), calls=%d", decision, reason, err, len(provider.requests))
	}
}

// changingEvidenceProvider records a failing test on its first call, so the
// unmet verification set changes and a second repair would be needed.
type changingEvidenceProvider struct{ calls int }

func (p *changingEvidenceProvider) Chat(ctx context.Context, _ *model.ChatRequest) (*model.ChatResponse, error) {
	p.calls++
	if p.calls == 1 {
		runtime, _ := taskRuntimeFromContext(ctx)
		exitCode := 1
		now := time.Now()
		_, _ = runtime.recordCommand("go test ./...", execution.CommandTest, &exitCode, execution.TerminalExited, nil, execution.ProvenanceRuntime, now, now)
	}
	return &model.ChatResponse{Role: model.RoleAssistant, Content: "tried"}, nil
}

func (p *changingEvidenceProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, nil
}
func (p *changingEvidenceProvider) Name() string  { return "changing" }
func (p *changingEvidenceProvider) Model() string { return "claude-haiku-4-5" }

func TestRepairAllowanceExhaustionIsAdvisoryInReportMode(t *testing.T) {
	for _, mode := range []verification.Mode{verification.ModeReport, verification.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			runtime, err := newTaskRuntimeWithLimits("task", t.TempDir(), execution.TaskLimits{RepairAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.recordWrite("main.go", "hash", 1, execution.ProvenanceRuntime, time.Now()); err != nil {
				t.Fatal(err)
			}
			a := newExecutionTestAgent("coder", &changingEvidenceProvider{})
			response, _, reason, err := (&Orchestrator{}).repairBlocking(withTaskRuntime(context.Background(), runtime), a, "", ExecutionRequest{VerificationMode: mode}, router.Classification{Kind: router.TaskKindEdit}, runtime, &model.ChatResponse{Content: "done"})
			if mode == verification.ModeEnforce {
				if reason != execution.StopBudgetExhausted || err == nil {
					t.Fatalf("enforce = (%q, %v), want budget exhaustion", reason, err)
				}
				return
			}
			if err != nil || reason != execution.StopSuccess || !strings.Contains(response.Content, "without current verification evidence") || !strings.Contains(response.Content, "test (failed)") {
				t.Fatalf("report = (%q, %q, %v)", response.Content, reason, err)
			}
		})
	}
}

func TestRepairPromptNamesConcreteCommands(t *testing.T) {
	runtime, err := newTaskRuntime("task", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	decision := verification.Decision{Obligations: []verification.Obligation{
		{ID: "test", Kind: verification.KindTest, Paths: []string{"main.go"}, Status: verification.StatusPending},
		{ID: "diff", Kind: verification.KindDiff, Paths: []string{"main.go"}, Status: verification.StatusPending},
	}}
	prompt := buildRepairPrompt(decision, runtime)
	for _, want := range []string{"`go test ./...`", "`git diff --stat`", "without pipes"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("repair prompt missing %q:\n%s", want, prompt)
		}
	}
}

// Renew mode disables task limits (0 = unlimited). Printing them as 0 made the
// model believe no tool calls remained and hand the task back to the user.
func TestRepairPromptRendersUnlimitedLimits(t *testing.T) {
	runtime, err := newTaskRuntimeWithLimits("task", t.TempDir(), execution.TaskLimits{ModelCalls: 5})
	if err != nil {
		t.Fatal(err)
	}
	prompt := buildRepairPrompt(verification.Decision{}, runtime)
	want := "Remaining limits: repairs=unlimited model_calls=5 tool_calls=unlimited tokens=unlimited cost_microdollars=unlimited"
	if !strings.Contains(prompt, want) {
		t.Fatalf("repair prompt missing %q:\n%s", want, prompt)
	}
	if !strings.Contains(prompt, "deliverable is still missing") {
		t.Fatalf("repair prompt should allow finishing missing deliverables:\n%s", prompt)
	}
}

func TestRuntimeVerificationSkipsDiffOutsideGit(t *testing.T) {
	for _, git := range []bool{false, true} {
		root := t.TempDir()
		if git {
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		runtime, err := newTaskRuntime("task", root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.recordWrite("main.go", "hash", 1, execution.ProvenanceRuntime, time.Now()); err != nil {
			t.Fatal(err)
		}
		decision := assessRuntimeVerification(ExecutionRequest{}, router.Classification{Kind: router.TaskKindEdit}, runtime)
		hasDiff := false
		for _, obligation := range decision.Obligations {
			hasDiff = hasDiff || obligation.Kind == verification.KindDiff
		}
		if hasDiff != git {
			t.Fatalf("git=%v: diff obligation present=%v, obligations=%#v", git, hasDiff, decision.Obligations)
		}
	}
}

func TestRuntimeVerificationDropsRefusedChecks(t *testing.T) {
	refusedTests := regexp.MustCompile(`^(go test|npm test|pytest|cargo test|make check)(\s|$)`)
	for _, tc := range []struct {
		name     string
		refuses  func(string) bool
		path     string
		wantTest bool
	}{
		{name: "no policy", path: "main.go", wantTest: true},
		{name: "language test command refused", refuses: refusedTests.MatchString, path: "main.go"},
		{name: "unrefused language test command", refuses: refusedTests.MatchString, path: "Main.java", wantTest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			runtime, err := newTaskRuntime("task", root)
			if err != nil {
				t.Fatal(err)
			}
			runtime.refusesShell = tc.refuses
			if _, err := runtime.recordWrite(tc.path, "hash", 1, execution.ProvenanceRuntime, time.Now()); err != nil {
				t.Fatal(err)
			}
			request := ExecutionRequest{VerificationObligations: []verification.Obligation{
				{ID: "test:make check", Kind: verification.KindTest, Command: "make check", Paths: []string{tc.path}, Status: verification.StatusPending},
			}}
			decision := assessRuntimeVerification(request, router.Classification{Kind: router.TaskKindEdit}, runtime)
			var hasTest, hasDiff bool
			for _, obligation := range decision.Obligations {
				hasTest = hasTest || obligation.Kind == verification.KindTest
				hasDiff = hasDiff || obligation.Kind == verification.KindDiff
			}
			if hasTest != tc.wantTest || !hasDiff {
				t.Fatalf("test obligation=%v diff obligation=%v, obligations=%#v", hasTest, hasDiff, decision.Obligations)
			}
		})
	}
}
