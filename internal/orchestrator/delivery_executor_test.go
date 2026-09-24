package orchestrator

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/execution"
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
	if !ok || identity.TenantID != "tenant" || identity.RepositoryID != "repo" || identity.TaskID != "delivery-1" {
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
