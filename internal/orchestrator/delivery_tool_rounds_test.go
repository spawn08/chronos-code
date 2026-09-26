package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/workspace"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

type toolRoundCrashClock struct{ timestamp atomic.Int64 }

func (c *toolRoundCrashClock) Now() time.Time { return time.Unix(0, c.timestamp.Load()).UTC() }

type toolRoundCrashProvider struct {
	chat func(context.Context, *model.ChatRequest) (*model.ChatResponse, error)
}

func (p toolRoundCrashProvider) Name() string  { return "fixture" }
func (p toolRoundCrashProvider) Model() string { return "fixture" }
func (p toolRoundCrashProvider) Chat(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	return p.chat(ctx, req)
}
func (p toolRoundCrashProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, errors.New("streaming not expected")
}

type crashAfterRound struct{}

func (crashAfterRound) AfterToolRound(context.Context, agent.ToolRound) (agent.ToolLoopAction, error) {
	return agent.ToolLoopAction{}, errors.New("injected crash after checkpoint")
}

func TestWorkerRestartResumesCheckpointedAssistantAndToolResult(t *testing.T) {
	for _, effectful := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "observed effect"}[effectful], func(t *testing.T) {
			testWorkerRestartResumesRound(t, effectful)
		})
	}
}

func testWorkerRestartResumesRound(t *testing.T, effectful bool) {
	t.Helper()
	ctx := context.Background()
	clock := &toolRoundCrashClock{}
	clock.timestamp.Store(time.Now().Add(time.Second).UnixNano())
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store, err := execution.OpenDeliveryStoreWithClock(ctx, path, clock)
	if err != nil {
		t.Fatal(err)
	}
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "request", Goal: execution.Goal{Statement: "inspect", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "old", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	providerCalls, toolCalls := 0, 0
	provider := toolRoundCrashProvider{chat: func(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
		providerCalls++
		if providerCalls == 1 {
			return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: "lookup-1", Name: "lookup", Arguments: `{}`}}, ProviderState: []json.RawMessage{json.RawMessage(`{"id":"provider-state"}`)}, Usage: model.Usage{PromptTokens: 5, CompletionTokens: 1}, UsageKnown: true}, nil
		}
		if len(req.Messages) < 3 || req.Messages[len(req.Messages)-2].ToolCalls[0].ID != "lookup-1" || req.Messages[len(req.Messages)-1].Content != `"evidence"` {
			t.Errorf("replacement lost assistant/tool exchange: %+v", req.Messages)
		}
		if _, ok := req.Messages[len(req.Messages)-2].ProviderState.([]json.RawMessage); !ok {
			t.Errorf("provider continuation type lost: %T", req.Messages[len(req.Messages)-2].ProviderState)
		}
		return &model.ChatResponse{Content: "completed from evidence", StopReason: model.StopReasonEnd, Usage: model.Usage{PromptTokens: 9, CompletionTokens: 2}, UsageKnown: true}, nil
	}}
	effects := []tool.Effect{tool.EffectRead}
	if effectful {
		effects = []tool.Effect{tool.EffectProcessExecution}
	}
	a, err := agent.New("reader", "Reader").WithModel(provider).AddTool(&tool.Definition{
		Name: "lookup", Permission: tool.PermAllow, Effects: effects,
		Handler: func(context.Context, map[string]any) (any, error) {
			toolCalls++
			return "evidence", nil
		},
	}).Build()
	if err != nil {
		t.Fatal(err)
	}
	a.Hooks = append(a.Hooks, deliveryBudgetHook{session: budgetHook{tracker: budget.NewTracker(0, 0), agentID: "reader"}})
	if effectful {
		wrapDeliveryOperations(a)
	}
	first := agent.WithRunIdentity(execution.WithOperationLease(ctx, store, lease), agent.RunIdentity{TaskID: "delivery", RoleID: "reader", InvocationID: "first"})
	if effectful {
		first = security.WithEffectGrant(first, security.EffectProcessExecution)
	}
	first = agent.WithToolRoundJournal(first, deliveryToolRoundJournal{attempt: &execution.Execution{Lease: lease}, roles: map[string]*agent.Agent{"reader": a}})
	first = agent.WithToolLoopController(first, "reader", crashAfterRound{})
	if _, err := a.Chat(first, "inspect"); err == nil || providerCalls != 1 || toolCalls != 1 {
		t.Fatalf("first attempt: provider=%d tool=%d error=%v", providerCalls, toolCalls, err)
	}
	usage, err := store.Usage(ctx, scope, "delivery")
	if err != nil || usage.ReconciledCalls != 1 {
		t.Fatalf("first model reply usage = %+v, error=%v", usage, err)
	}
	if effectful {
		op, err := store.Operation(ctx, scope, "delivery", "first:lookup-1")
		if err != nil || op.Status != execution.OperationObserved {
			t.Fatalf("effect receipt = %+v, error=%v", op, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	clock.timestamp.Add(int64(time.Minute + time.Second))
	store, err = execution.OpenDeliveryStoreWithClock(ctx, path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	orch := &Orchestrator{agents: map[string]*agent.Agent{"reader": a}, active: "reader", workspace: &workspace.Info{Root: t.TempDir()}}
	executor, err := NewReadOnlyDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(store, executor, execution.WorkerConfig{OwnerID: "replacement", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("replacement worker: %v", err)
	}
	if providerCalls != 2 || toolCalls != 1 {
		t.Fatalf("replacement repeated exchange: provider=%d tool=%d", providerCalls, toolCalls)
	}
	usage, err = store.Usage(ctx, scope, "delivery")
	if err != nil || usage.ReconciledCalls != 2 {
		t.Fatalf("recovered model usage = %+v, error=%v", usage, err)
	}
	attempts, err := store.Attempts(ctx, scope, "delivery")
	if err != nil || len(attempts) != 2 || string(attempts[1].Result) == "" {
		t.Fatalf("replacement result = %+v, error=%v", attempts, err)
	}
}
