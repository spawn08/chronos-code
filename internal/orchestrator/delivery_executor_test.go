package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/workspace"
)

func TestWorkerOwnedReadOnlyDeliveryUsesCommonExecutorAndParksUnverifiedResult(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	admitted, err := store.Admit(ctx, execution.Admission{
		Scope: scope, DeliveryID: "delivery-1", AdmissionKey: "request-1",
		Goal:         execution.Goal{Statement: "inspect the repository", Actor: "user"},
		Requirements: []execution.Requirement{{ID: "requirement-1", Statement: "report evidence", Status: execution.RequirementAccepted}},
		Event:        execution.EventIdentity{ID: "admit-1", IdempotencyKey: "admit-key-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueueAdmitted(ctx, scope, admitted.ID, admitted.Version); err != nil {
		t.Fatal(err)
	}
	provider := &executionTestProvider{name: "read-only", modelID: "test-model"}
	orch := &Orchestrator{agents: map[string]*agent.Agent{"reader": newExecutionTestAgent("reader", provider)}, active: "reader", workspace: &workspace.Info{Root: t.TempDir()}}
	authorizer := authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}
	executor, err := NewReadOnlyDeliveryExecutor(orch, authorizer, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(store, executor, execution.WorkerConfig{OwnerID: "worker-1", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	clientCtx, cancelClient := context.WithCancel(ctx)
	cancelClient() // A detached client cannot cancel the worker-owned context.
	if clientCtx.Err() == nil {
		t.Fatal("test client was not detached")
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	identity, ok := agent.RunIdentityFromContext(provider.executionContext(0))
	if !ok || identity.TenantID != "tenant" || identity.RepositoryID != "repo" || identity.TaskID != "delivery-1" || identity.DeliveryID != "delivery-1" || identity.ParentInvocationID == "" {
		t.Fatalf("worker run identity = %+v, ok = %v", identity, ok)
	}
	grant, ok := security.EffectGrantFromContext(provider.executionContext(0))
	if !ok || len(grant) != 1 {
		t.Fatalf("worker effect grant = %v, ok = %v", grant, ok)
	}
	if _, ok := grant[tool.EffectRead]; !ok {
		t.Fatalf("worker granted effects = %v", grant)
	}
	loaded, err := store.Load(ctx, scope, admitted.ID)
	if err != nil || loaded.State != execution.DeliveryWaitingDecision {
		t.Fatalf("unverified delivery = %+v, error = %v", loaded, err)
	}
	attempts, err := store.Attempts(ctx, scope, admitted.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != execution.OutcomeWaiting {
		t.Fatalf("worker attempts = %+v, error = %v", attempts, err)
	}
	mutatingGoal, err := store.Admit(ctx, execution.Admission{
		Scope: scope, DeliveryID: "memory-intent", AdmissionKey: "request-2",
		Goal:  execution.Goal{Statement: "remember: persist this note", Actor: "user"},
		Event: execution.EventIdentity{ID: "admit-2", IdempotencyKey: "admit-key-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueueAdmitted(ctx, scope, mutatingGoal.ID, mutatingGoal.Version); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(provider.contexts) != 1 {
		t.Fatal("read-only delivery executed a mutating memory intent")
	}
}

func TestReadOnlyDeliveryWorkerCanDelegateToConfiguredChild(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{
		Scope: scope, DeliveryID: "delivery", AdmissionKey: "request",
		Goal: execution.Goal{Statement: "inspect with child", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"},
	}); err != nil {
		t.Fatal(err)
	}
	var parentCalls int
	parent := resourceAgent(t, "parent", resourceProvider{chat: func(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
		parentCalls++
		if parentCalls == 1 {
			return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: "delegate", Name: "spawn_subagent", Arguments: `{"agent":"child","task":"inspect"}`}}}, nil
		}
		return resourceReply("inspection complete"), nil
	}})
	var childCtx context.Context
	child := resourceAgent(t, "child", resourceProvider{chat: func(ctx context.Context, _ *model.ChatRequest) (*model.ChatResponse, error) {
		childCtx = ctx
		return resourceReply("inspected"), nil
	}})
	parent.SubAgents = []*agent.Agent{child}
	if err := setupSubAgents(map[string]*agent.Agent{"parent": parent, "child": child}); err != nil {
		t.Fatal(err)
	}
	orch := &Orchestrator{agents: map[string]*agent.Agent{"parent": parent, "child": child}, active: "parent", workspace: &workspace.Info{Root: t.TempDir()}}
	executor, err := NewReadOnlyDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(store, executor, execution.WorkerConfig{OwnerID: "worker", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if childCtx == nil || parentCalls != 2 {
		t.Fatalf("child context = %v, parent calls = %d", childCtx, parentCalls)
	}
	identity, ok := agent.RunIdentityFromContext(childCtx)
	if !ok || identity.DeliveryID != "delivery" || identity.TaskID != "delivery" || identity.RoleID != "child" || identity.ParentInvocationID == "" {
		t.Fatalf("worker child identity = %+v, ok=%v", identity, ok)
	}
	grant, ok := security.EffectGrantFromContext(childCtx)
	if !ok || len(grant) != 0 {
		t.Fatalf("child gained effects outside its tool set: %v, ok=%v", grant, ok)
	}
	loaded, err := store.Load(ctx, scope, "delivery")
	if err != nil || loaded.State != execution.DeliveryWaitingDecision {
		t.Fatalf("delegated delivery = %+v, error = %v", loaded, err)
	}
}

type deliveryClock struct{ timestamp atomic.Int64 }

func (c *deliveryClock) Now() time.Time { return time.Unix(0, c.timestamp.Load()).UTC() }

func TestWorkerRestartParksPriorUnknownEffectBeforeModelReplay(t *testing.T) {
	ctx := context.Background()
	clock := &deliveryClock{}
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
	first, err := store.Claim(ctx, "old-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	op := execution.Operation{ID: "effect-1", EffectKey: "key-1", Kind: "shell", ReplayClass: execution.ReplayUnknown, InputFingerprint: "before"}
	if _, err := store.PrepareOperation(ctx, first, op); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginOperation(ctx, first, op.ID); err != nil {
		t.Fatal(err)
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
	provider := &executionTestProvider{name: "should-not-run", modelID: "fixture"}
	orch := &Orchestrator{agents: map[string]*agent.Agent{"reader": newExecutionTestAgent("reader", provider)}, active: "reader", workspace: &workspace.Info{Root: t.TempDir()}}
	authorizer := authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}
	executor, err := NewReadOnlyDeliveryExecutor(orch, authorizer, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(store, executor, execution.WorkerConfig{OwnerID: "replacement", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(provider.contexts) != 0 {
		t.Fatal("unreconciled effect was replayed through model")
	}
	loaded, err := store.Load(ctx, scope, "delivery")
	if err != nil || loaded.State != execution.DeliveryWaitingDecision {
		t.Fatalf("reclaimed delivery = %+v, error = %v", loaded, err)
	}
	if err := store.PublishResult(ctx, first, []byte(`{"unsafe":true}`)); err != execution.ErrStaleLease {
		t.Fatalf("old owner published result after reclaim: %v", err)
	}
}

func TestWorkerRestartObservesCompletedFileWriteWithoutReplayingIt(t *testing.T) {
	ctx := context.Background()
	clock := &deliveryClock{}
	clock.timestamp.Store(time.Now().UTC().Add(time.Second).UnixNano())
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store, err := execution.OpenDeliveryStoreWithClock(ctx, path, clock)
	if err != nil {
		t.Fatal(err)
	}
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "request", Goal: execution.Goal{Statement: "write output", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "old-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	provider := &deliveryToolProvider{}
	a := newExecutionTestAgent("writer", provider)
	a.Tools.Register(&tool.Definition{Name: "file_write", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectDeliveryWrite}, Handler: func(_ context.Context, args map[string]any) (any, error) {
		if err := os.WriteFile(filepath.Join(root, "output"), []byte(args["content"].(string)), 0o600); err != nil {
			return nil, err
		}
		return map[string]any{"path": "output"}, store.Close() // effect committed; observation persistence fails
	}})
	wrapDeliveryOperations(a)
	ctx = builtins.WithWorkspaceRoot(ctx, root)
	ctx = agent.WithRunIdentity(ctx, agent.RunIdentity{TaskID: "delivery", RoleID: "writer", InvocationID: "old-invocation"})
	ctx = security.WithEffectGrant(ctx, security.EffectDeliveryWrite)
	ctx = execution.WithOperationLease(ctx, store, lease)
	if _, err := a.Chat(ctx, "write output"); err == nil || provider.calls != 1 {
		t.Fatalf("ambiguous first attempt: calls = %d, error = %v", provider.calls, err)
	}
	clock.timestamp.Add(int64(time.Minute + time.Second))
	reopened, err := execution.OpenDeliveryStoreWithClock(context.Background(), path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reader := &executionTestProvider{name: "reader", modelID: "test"}
	orch := &Orchestrator{agents: map[string]*agent.Agent{"reader": newExecutionTestAgent("reader", reader)}, active: "reader", workspace: &workspace.Info{Root: root}}
	executor, err := NewReadOnlyDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(reopened, executor, execution.WorkerConfig{OwnerID: "replacement", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, err := reopened.Operation(context.Background(), scope, "delivery", "old-invocation:call-1")
	if err != nil || op.Status != execution.OperationReconciled || op.OutputFingerprint != op.ExpectedOutputFingerprint || op.Error == "" {
		t.Fatalf("observed write = %+v, error = %v", op, err)
	}
	if len(reader.contexts) != 0 {
		t.Fatal("prior write caused model replay")
	}
	if data, err := os.ReadFile(filepath.Join(root, "output")); err != nil || string(data) != "done" {
		t.Fatalf("recovered file = %q, error = %v", data, err)
	}
}

func TestCappedReadOnlyWorkerReservesAndReconcilesActualProviderUsage(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", MaxCostMicrodollars: 20000, Goal: execution.Goal{Statement: "inspect", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	provider := &executionTestProvider{name: "priced", modelID: "claude-sonnet-4-6", usage: model.Usage{PromptTokens: 7, CompletionTokens: 3}, known: true}
	a := newExecutionTestAgent("reader", provider)
	orch := &Orchestrator{agents: map[string]*agent.Agent{"reader": a}, active: "reader", workspace: &workspace.Info{Root: t.TempDir()}}
	a.Hooks = append(a.Hooks, deliveryBudgetHook{session: budgetHook{tracker: budget.NewTracker(0, 0), orchestrator: orch, agentID: "reader"}})
	executor, err := NewReadOnlyDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(store, executor, execution.WorkerConfig{OwnerID: "worker", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil || !worker.CanRunCapped() {
		t.Fatalf("capped worker = %+v, error = %v", worker, err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	usage, err := store.Usage(ctx, scope, "delivery")
	if err != nil || usage.InputTokens != 7 || usage.OutputTokens != 3 || usage.SpentMicrodollars != 66 || usage.ReservedMicrodollars != 0 || usage.OutstandingCalls != 0 || !usage.CostKnown || usage.ActiveNanoseconds <= 0 || usage.ProviderNanoseconds <= 0 {
		t.Fatalf("capped worker usage = %+v, error = %v", usage, err)
	}
	request := provider.request(0)
	if request.MaxTokens != 1024 {
		t.Fatalf("provider output allowance = %d, want 1024", request.MaxTokens)
	}
	loaded, err := store.Load(ctx, scope, "delivery")
	if err != nil || loaded.State != execution.DeliveryWaitingDecision {
		t.Fatalf("capped delivery = %+v, error = %v", loaded, err)
	}
}

func TestReadOnlyDeliveryDoesNotPersistEpisodeOutsideEffectGrant(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	layers, err := memory.OpenLayerStore(ctx, filepath.Join(root, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer layers.Close()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(root, "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", Goal: execution.Goal{Statement: "inspect", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	provider := &executionTestProvider{name: "reader", modelID: "test"}
	orch := &Orchestrator{agents: map[string]*agent.Agent{"reader": newExecutionTestAgent("reader", provider)}, active: "reader", workspace: &workspace.Info{Root: root},
		runtimeMemory: &runtimeMemory{store: layers, options: memory.LayerOptions{ProjectID: "project", Budget: memory.RetrievalBudget{MaxRecords: 10, MaxBytes: 10000}}}}
	executor, err := NewReadOnlyDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(store, executor, execution.WorkerConfig{OwnerID: "worker", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	records, err := layers.Recall(ctx, orch.runtimeMemory.options, memory.LayerQuery{Scope: memory.ScopeProject, Kind: memory.KindEpisodic})
	if err != nil || len(records) != 0 {
		t.Fatalf("read-only worker wrote episodic memory = %+v, error = %v", records, err)
	}
}
