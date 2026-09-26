package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/team"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/workspace"
)

func testDurableParallelTeamOrchestrator(t *testing.T) (*Orchestrator, *executionTestProvider, *executionTestProvider, *agent.Agent) {
	t.Helper()
	first := &executionTestProvider{name: "first", modelID: "claude-sonnet-4-6", usage: model.Usage{PromptTokens: 2, CompletionTokens: 1}, known: true}
	second := &executionTestProvider{name: "second", modelID: "claude-sonnet-4-6", usage: model.Usage{PromptTokens: 3, CompletionTokens: 1}, known: true}
	firstAgent := newExecutionTestAgent("first", first)
	secondAgent := newExecutionTestAgent("second", second)
	roles := map[string]*agent.Agent{"first": firstAgent, "second": secondAgent}
	members := team.New("fan", "Fan", team.StrategyParallel).AddAgent(firstAgent).AddAgent(secondAgent)
	orch := &Orchestrator{agents: roles, active: "first", teams: map[string]*team.Team{"fan": members}, workspace: &workspace.Info{Root: t.TempDir()}}
	for id, a := range roles {
		a.Hooks = append(a.Hooks, deliveryBudgetHook{session: budgetHook{tracker: budget.NewTracker(0, 0), orchestrator: orch, agentID: id}})
	}
	return orch, first, second, secondAgent
}

func admitParallelTeam(t *testing.T, store *execution.DeliveryStore, scope execution.DeliveryScope) {
	t.Helper()
	if _, err := store.AdmitRunnable(context.Background(), execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", PolicyReference: execution.ReadOnlyTeamPolicyPrefix + "fan",
		Goal: execution.Goal{Statement: "inspect in parallel", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
}

func newTeamWorker(t *testing.T, orch *Orchestrator, store *execution.DeliveryStore, owner string) *execution.Worker {
	t.Helper()
	executor, err := NewReadOnlyDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(store, executor, execution.WorkerConfig{OwnerID: owner, Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func loadParallelCheckpoint(t *testing.T, store *execution.DeliveryStore, scope execution.DeliveryScope, attempt int) parallelTeamCheckpoint {
	t.Helper()
	attempts, err := store.Attempts(context.Background(), scope, "delivery")
	if err != nil || len(attempts) <= attempt {
		t.Fatalf("attempts = %+v, error = %v", attempts, err)
	}
	var checkpoint parallelTeamCheckpoint
	if len(attempts[attempt].Checkpoint) != 0 {
		if err := json.Unmarshal(attempts[attempt].Checkpoint, &checkpoint); err != nil {
			t.Fatal(err)
		}
	}
	return checkpoint
}

func TestReadOnlyParallelTeamWorkerRecordsPerMemberReceipts(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	admitParallelTeam(t, store, scope)
	orch, first, second, _ := testDurableParallelTeamOrchestrator(t)
	if err := newTeamWorker(t, orch, store, "worker").RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(first.contexts) != 1 || len(second.contexts) != 1 {
		t.Fatalf("member calls = first %d second %d", len(first.contexts), len(second.contexts))
	}
	identity, ok := agent.RunIdentityFromContext(second.executionContext(0))
	if !ok || identity.NodeID != "team:fan:1" || identity.RoleID != "second" || identity.DeliveryID != "delivery" {
		t.Fatalf("second member identity = %+v", identity)
	}
	checkpoint := loadParallelCheckpoint(t, store, scope, 0)
	if checkpoint.Version != 2 || len(checkpoint.Members) != 2 || checkpoint.Members[0] != (memberReceipt{Response: "first", ReconciledCalls: 1}) || checkpoint.Members[1] != (memberReceipt{Response: "second", ReconciledCalls: 1}) {
		t.Fatalf("parallel receipts = %+v", checkpoint)
	}
	byNode, err := store.UsageByNode(ctx, scope, "delivery")
	if err != nil || byNode["team:fan:0"].Reconciled != 1 || byNode["team:fan:1"].Reconciled != 1 || len(byNode) != 2 {
		t.Fatalf("usage by node = %+v, error = %v", byNode, err)
	}
	loaded, err := store.Load(ctx, scope, "delivery")
	if err != nil || loaded.State != execution.DeliveryWaitingDecision {
		t.Fatalf("parallel team result = %+v, error = %v", loaded, err)
	}
}

// cancelAfterPeerReceipt blocks a member's model call until the other member's
// receipt is durable, then cancels the worker before the call is reserved.
type cancelAfterPeerReceipt struct {
	store  *execution.DeliveryStore
	scope  execution.DeliveryScope
	cancel context.CancelFunc
}

func (h cancelAfterPeerReceipt) Before(ctx context.Context, event *hooks.Event) error {
	if event.Type != hooks.EventModelCallBefore {
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		attempts, err := h.store.Attempts(context.Background(), h.scope, "delivery")
		if err == nil && len(attempts) == 1 && len(attempts[0].Checkpoint) != 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.cancel()
	return context.Canceled
}
func (cancelAfterPeerReceipt) After(context.Context, *hooks.Event) error { return nil }

func TestReadOnlyParallelTeamRestartRunsOnlyMemberWithoutReceipt(t *testing.T) {
	clock := &deliveryClock{}
	clock.timestamp.Store(time.Now().Add(time.Second).UnixNano())
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store, err := execution.OpenDeliveryStoreWithClock(context.Background(), path, clock)
	if err != nil {
		t.Fatal(err)
	}
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	admitParallelTeam(t, store, scope)
	orch, first, second, secondAgent := testDurableParallelTeamOrchestrator(t)
	ctx, cancel := context.WithCancel(context.Background())
	secondAgent.Hooks = append(hooks.Chain{cancelAfterPeerReceipt{store: store, scope: scope, cancel: cancel}}, secondAgent.Hooks...)
	if err := newTeamWorker(t, orch, store, "old").RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted parallel team = %v", err)
	}
	if checkpoint := loadParallelCheckpoint(t, store, scope, 0); len(checkpoint.Members) != 1 || checkpoint.Members[0].Response != "first" {
		t.Fatalf("receipts before restart = %+v", checkpoint)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	clock.timestamp.Add(int64(time.Minute + time.Second))
	store, err = execution.OpenDeliveryStoreWithClock(context.Background(), path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secondAgent.Hooks = secondAgent.Hooks[1:]
	if err := newTeamWorker(t, orch, store, "replacement").RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(first.contexts) != 1 || len(second.contexts) != 1 {
		t.Fatalf("restart resubmitted a checkpointed member: first=%d second=%d", len(first.contexts), len(second.contexts))
	}
	checkpoint := loadParallelCheckpoint(t, store, scope, 1)
	if len(checkpoint.Members) != 2 || checkpoint.Members[1] != (memberReceipt{Response: "second", ReconciledCalls: 1}) {
		t.Fatalf("receipts after restart = %+v", checkpoint)
	}
	loaded, err := store.Load(context.Background(), scope, "delivery")
	if err != nil || loaded.State != execution.DeliveryWaitingDecision {
		t.Fatalf("resumed parallel team = %+v, error = %v", loaded, err)
	}
	attempts, err := store.Attempts(context.Background(), scope, "delivery")
	var result struct {
		Content string `json:"content"`
	}
	if err != nil || len(attempts) != 2 || json.Unmarshal(attempts[1].Result, &result) != nil || result.Content != "first\n\n---\n\nsecond" {
		t.Fatalf("merged result = %+v, attempts = %+v, error = %v", result, attempts, err)
	}
}

func TestReadOnlyParallelTeamParksBilledMemberWithoutReceipt(t *testing.T) {
	clock := &deliveryClock{}
	clock.timestamp.Store(time.Now().Add(time.Second).UnixNano())
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store, err := execution.OpenDeliveryStoreWithClock(context.Background(), path, clock)
	if err != nil {
		t.Fatal(err)
	}
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	admitParallelTeam(t, store, scope)
	orch, first, second, secondAgent := testDurableParallelTeamOrchestrator(t)
	ctx, cancel := context.WithCancel(context.Background())
	secondAgent.Hooks = append(hooks.Chain{cancelAfterTeamModelHook{cancel: cancel}}, secondAgent.Hooks...)
	if err := newTeamWorker(t, orch, store, "old").RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupt between model call and receipt = %v", err)
	}
	byNode, err := store.UsageByNode(context.Background(), scope, "delivery")
	if err != nil || byNode["team:fan:1"].Reconciled != 1 {
		t.Fatalf("billed second member = %+v, error = %v", byNode, err)
	}
	if checkpoint := loadParallelCheckpoint(t, store, scope, 0); checkpoint.Members[1] != (memberReceipt{}) {
		t.Fatalf("second member was checkpointed despite interruption: %+v", checkpoint)
	}
	firstCalls := len(first.contexts)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	clock.timestamp.Add(int64(time.Minute + time.Second))
	store, err = execution.OpenDeliveryStoreWithClock(context.Background(), path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secondAgent.Hooks = secondAgent.Hooks[1:]
	if err := newTeamWorker(t, orch, store, "replacement").RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(second.contexts) != 1 || len(first.contexts) != firstCalls {
		t.Fatalf("uncheckpointed billed member was replayed: first=%d->%d second=%d", firstCalls, len(first.contexts), len(second.contexts))
	}
	attempts, err := store.Attempts(context.Background(), scope, "delivery")
	if err != nil || len(attempts) != 2 || attempts[1].Error == "" {
		t.Fatalf("recovery decision = %+v, error = %v", attempts, err)
	}
	loaded, err := store.Load(context.Background(), scope, "delivery")
	if err != nil || loaded.State != execution.DeliveryWaitingDecision {
		t.Fatalf("parked parallel team = %+v, error = %v", loaded, err)
	}
}
