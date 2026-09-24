package plan

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestPlanLeaseExpiryParksUnknownEffectsAndFencesLateWorker(t *testing.T) {
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	ctx, scheduler, p := schedulerPlan(t, []Node{{ID: "a", State: NodePending}, {ID: "b", State: NodePending}}, nil,
		SchedulerConfig{LeaseDuration: 5 * time.Second, Now: func() time.Time { return now }})
	if _, err := scheduler.Claim(ctx, p, claimRequest("a")); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Start(ctx, p, "a", "lease-a"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Second)
	if err := scheduler.Heartbeat(ctx, p, "a", "lease-a"); err != nil {
		t.Fatalf("renew live lease: %v", err)
	}
	now = now.Add(6 * time.Second)
	if err := scheduler.Heartbeat(ctx, p, "a", "lease-a"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired owner renewed lease: %v", err)
	}
	if _, err := scheduler.Ready(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Complete(ctx, p, "a", "lease-a", "late-completion", "late-completion"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker finalized node: %v", err)
	}
	loaded, err := scheduler.store.Load(ctx, p)
	if err != nil || loaded.State != PlanPaused || loaded.StopReason != StopAmbiguity || loaded.Nodes[0].State != NodeBlocked || len(loaded.Leases) != 0 {
		t.Fatalf("expired plan = %+v, error = %v", loaded, err)
	}
	if _, err := scheduler.Claim(ctx, p, claimRequest("b")); !errors.Is(err, ErrNoReadyNode) {
		t.Fatalf("plan scheduled further effects before reconciliation: %v", err)
	}
}

func TestPlanLegacyLeaseIsParkedInsteadOfAutomaticallyReplayed(t *testing.T) {
	ctx, scheduler, p := schedulerPlan(t, []Node{{ID: "a", State: NodePending}}, nil, SchedulerConfig{})
	if _, err := scheduler.Claim(ctx, p, claimRequest("a")); err != nil {
		t.Fatal(err)
	}
	path := scheduler.store.path
	if _, err := scheduler.store.db.ExecContext(ctx, `ALTER TABLE plan_leases DROP COLUMN expires_at`); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.store.db.ExecContext(ctx, `DELETE FROM plan_schema_migrations WHERE version = 4`); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scheduler = NewScheduler(store, SchedulerConfig{})
	if _, err := scheduler.Ready(ctx, p); err != nil {
		t.Fatal(err)
	}
	loaded, err := scheduler.store.Load(ctx, p)
	if err != nil || loaded.State != PlanPaused || loaded.Nodes[0].State != NodeBlocked {
		t.Fatalf("legacy lease = %+v, error = %v", loaded, err)
	}
}

func TestControllerRenewsLeaseDuringLongNodeExecution(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	p := Plan{TenantID: "tenant", RepositoryID: "repo", TaskID: "task", ID: "plan", Generation: "one", State: PlanActive, Nodes: []Node{{ID: "a", State: NodePending}}}
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	controller := NewController(store, nodeExecutorFunc(func(ctx context.Context, request NodeExecutionRequest) (NodeExecutionResult, error) {
		select {
		case <-ctx.Done():
			return NodeExecutionResult{}, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		return NodeExecutionResult{NodeID: request.Node.ID, AttemptID: request.Attempt, Status: NodeCompleted, Verification: VerificationPassed}, nil
	}), nil, nil, ControllerConfig{Scheduler: SchedulerConfig{LeaseDuration: 150 * time.Millisecond}})
	result, err := controller.Run(ctx, p)
	if err != nil || result.State != PlanCompleted {
		t.Fatalf("long node after heartbeat = %+v, error = %v", result, err)
	}
}

func TestPlanReaperRecoversExpiredOwnersAcrossRestartWithoutTouchingLiveTenants(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "plans.db")
	store, err := OpenSQLStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	scheduler := NewScheduler(store, SchedulerConfig{LeaseDuration: 5 * time.Minute, Now: func() time.Time { return now }})
	first := Plan{TenantID: "tenant-a", RepositoryID: "repo", TaskID: "task", ID: "plan", Generation: "one", State: PlanActive, Nodes: []Node{{ID: "node", State: NodePending}}}
	second := first
	second.TenantID = "tenant-b"
	for _, p := range []Plan{first, second} {
		if err := store.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := scheduler.Claim(ctx, first, claimRequest("first")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute)
	if _, err := scheduler.Claim(ctx, second, claimRequest("second")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenSQLStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now = now.Add(3 * time.Minute)
	reaped, err := store.ReapExpiredLeases(ctx, now, 100)
	if err != nil || reaped != 1 {
		t.Fatalf("reaped generations = %d, error = %v", reaped, err)
	}
	for _, test := range []struct {
		plan Plan
		want PlanState
	}{
		{first, PlanPaused}, {second, PlanActive},
	} {
		loaded, err := store.Load(ctx, test.plan)
		if err != nil || loaded.State != test.want {
			t.Fatalf("tenant %s state = %s, want %s, error = %v", test.plan.TenantID, loaded.State, test.want, err)
		}
	}
	if again, err := store.ReapExpiredLeases(ctx, now, 100); err != nil || again != 0 {
		t.Fatalf("duplicate recovery = %d, error = %v", again, err)
	}
}
