package plan

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestSchedulerClaimsOnlyDependencyReadyNode(t *testing.T) {
	ctx, scheduler, p := schedulerPlan(t, []Node{{ID: "a", State: NodePending}, {ID: "b", State: NodePending}}, []Dependency{{NodeID: "b", DependsOn: "a"}}, SchedulerConfig{})
	claimed, err := scheduler.Claim(ctx, p, claimRequest("a"))
	if err != nil || claimed.ID != "a" {
		t.Fatalf("Claim() = (%+v, %v), want a", claimed, err)
	}
	if _, err := scheduler.Claim(ctx, p, claimRequest("b")); !errors.Is(err, ErrNoReadyNode) {
		t.Fatalf("Claim before predecessor completion error = %v, want %v", err, ErrNoReadyNode)
	}
	if err := scheduler.Start(ctx, p, "a", "lease-a"); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Complete(ctx, p, "a", "lease-a", "complete-a", "complete-a"); err != nil {
		t.Fatal(err)
	}
	ready, err := scheduler.Ready(ctx, p)
	if err != nil || len(ready) != 1 || ready[0].ID != "b" {
		t.Fatalf("Ready() = (%+v, %v), want b", ready, err)
	}
}

func TestSchedulerClaimHasOneWinner(t *testing.T) {
	ctx, scheduler, p := schedulerPlan(t, []Node{{ID: "a", State: NodePending}}, nil, SchedulerConfig{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := scheduler.Claim(ctx, p, claimRequest(id))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrNoReadyNode) {
			t.Fatalf("Claim() error = %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners = %d, want 1", winners)
	}
}

func TestSchedulerCompletionDerivesPlanState(t *testing.T) {
	ctx, scheduler, p := schedulerPlan(t, []Node{{ID: "a", State: NodePending}, {ID: "b", State: NodePending}}, []Dependency{{NodeID: "b", DependsOn: "a"}}, SchedulerConfig{})
	claimAndStart(t, ctx, scheduler, p, "a")
	if err := scheduler.Complete(ctx, p, "a", "lease-a", "complete-a", "complete-a"); err != nil {
		t.Fatal(err)
	}
	claimAndStart(t, ctx, scheduler, p, "b")
	if err := scheduler.Complete(ctx, p, "b", "lease-b", "complete-b", "complete-b"); err != nil {
		t.Fatal(err)
	}
	got, err := scheduler.store.Load(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != PlanCompleted || got.Nodes[1].State != NodeCompleted {
		t.Fatalf("completed plan = %#v", got)
	}
}

func TestSchedulerRetryBoundsAndTerminalPropagation(t *testing.T) {
	ctx, scheduler, p := schedulerPlan(t, []Node{{ID: "a", State: NodePending}, {ID: "b", State: NodePending}}, []Dependency{{NodeID: "b", DependsOn: "a"}}, SchedulerConfig{MaxAttempts: 2})
	claimAndStart(t, ctx, scheduler, p, "a")
	if err := scheduler.Retry(ctx, p, "a", "lease-a", "retry-a", "retry-a"); err != nil {
		t.Fatal(err)
	}
	claimed, err := scheduler.Claim(ctx, p, claimRequest("a-second"))
	if err != nil || claimed.ID != "a" {
		t.Fatalf("second Claim() = (%+v, %v), want a", claimed, err)
	}
	if err := scheduler.Start(ctx, p, "a", "lease-a-second"); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Heartbeat(ctx, p, "a", "lease-a"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Heartbeat with superseded lease error = %v, want %v", err, ErrLeaseLost)
	}
	if err := scheduler.Retry(ctx, p, "a", "lease-a-second", "retry-b", "retry-b"); err != nil {
		t.Fatal(err)
	}
	got, err := scheduler.store.Load(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != PlanFailed || got.Nodes[0].State != NodeFailed || got.Nodes[1].State != NodeBlocked {
		t.Fatalf("retry exhaustion = %#v", got)
	}
}

func TestSchedulerCancellationAndLeaseFence(t *testing.T) {
	ctx, scheduler, p := schedulerPlan(t, []Node{{ID: "a", State: NodePending}, {ID: "b", State: NodePending}}, nil, SchedulerConfig{MaxConcurrent: 1})
	claimAndStart(t, ctx, scheduler, p, "a")
	if _, err := scheduler.Claim(ctx, p, claimRequest("b")); !errors.Is(err, ErrNoReadyNode) {
		t.Fatalf("Claim over concurrency bound error = %v, want %v", err, ErrNoReadyNode)
	}
	if err := scheduler.Heartbeat(ctx, p, "a", "other"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Heartbeat() error = %v, want %v", err, ErrLeaseLost)
	}
	if err := scheduler.Cancel(ctx, p, "a", "lease-a", "cancel-a", "cancel-a"); err != nil {
		t.Fatal(err)
	}
	got, err := scheduler.store.Load(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != PlanCanceled || got.Nodes[1].State != NodeCanceled {
		t.Fatalf("canceled plan = %#v", got)
	}
}

func schedulerPlan(t *testing.T, nodes []Node, edges []Dependency, config SchedulerConfig) (context.Context, *Scheduler, Plan) {
	t.Helper()
	ctx := context.Background()
	store := openTestSQLStore(t)
	p := Plan{TenantID: "tenant", RepositoryID: "repo", TaskID: "task", ID: "plan", Generation: "one", State: PlanActive, Nodes: nodes, Dependencies: edges}
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	return ctx, NewScheduler(store, config), p
}

func claimRequest(id string) ClaimRequest {
	return ClaimRequest{AttemptID: AttemptID("attempt-" + id), LeaseID: LeaseID("lease-" + id), EventID: EventID("claim-" + id), IdempotencyKey: IdempotencyKey("claim-" + id)}
}

func claimAndStart(t *testing.T, ctx context.Context, scheduler *Scheduler, p Plan, id string) {
	t.Helper()
	claimed, err := scheduler.Claim(ctx, p, claimRequest(id))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != NodeID(id) {
		t.Fatalf("claimed node = %q, want %q", claimed.ID, id)
	}
	if err := scheduler.Start(ctx, p, NodeID(id), LeaseID("lease-"+id)); err != nil {
		t.Fatal(err)
	}
}
