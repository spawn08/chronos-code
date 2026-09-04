package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/router"
)

func TestDelegation(t *testing.T) {
	ctx := context.Background()
	fixture := loadDelegationFixture(t)
	store, err := plan.OpenSQLStore(ctx, filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	policy := router.NewPPDPolicy(router.PPDConfig{
		Version: "v1", Mode: router.PPDModeEnabled, Specialist: "ppd-planner", MaxPlannerCalls: 1,
		Thresholds: router.PPDThresholds{MinFiles: 3, MinPackages: 2, MinEstimatedCalls: 5},
	}, nil)
	planner := delegationPlanner{store: store, fixture: fixture}

	simple := policy.Decide(fixture.simpleRequest())
	if simple.Action != router.PPDActionBypass || planner.calls != 0 {
		t.Fatalf("simple task decision = %+v, planner calls = %d; want bypass without planner call", simple, planner.calls)
	}

	complex := policy.Decide(fixture.complexRequest(0))
	if complex.Action != router.PPDActionDelegate {
		t.Fatalf("complex task decision = %+v, want delegation", complex)
	}
	child, err := planner.Delegate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if child.TaskID != plan.TaskID(fixture.Complex.TaskID) || child.ID == "" || child.Generation == "" || child.ValidateDAG() != nil {
		t.Fatalf("delegated child is not a valid durable DAG: %#v", child)
	}

	executor := &delegationExecutor{}
	controller := plan.NewController(store, executor, nil, nil, plan.ControllerConfig{Scheduler: plan.SchedulerConfig{MaxAttempts: 1}})
	completed, err := controller.Run(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != plan.PlanCompleted || executor.calls != len(fixture.Complex.Nodes) {
		t.Fatalf("delegated child = %#v, executions = %d", completed, executor.calls)
	}
	persisted, err := store.Load(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ID != child.ID || persisted.Generation != child.Generation || len(persisted.Attempts) != len(fixture.Complex.Nodes) {
		t.Fatalf("child identity was not durably retained: %#v", persisted)
	}

	assertDelegationBudget(t)
	assertDelegationRestartCancellationAndRecursion(t, ctx, store, policy, fixture)
	assertDelegationRestart(t, ctx, fixture)
}

type delegationFixture struct {
	TenantID     string `json:"tenant_id"`
	RepositoryID string `json:"repository_id"`
	Simple       struct {
		TaskID         string `json:"task_id"`
		FileCount      int    `json:"file_count"`
		PackageCount   int    `json:"package_count"`
		EstimatedCalls int    `json:"estimated_calls"`
	} `json:"simple"`
	Complex struct {
		TaskID         string   `json:"task_id"`
		FileCount      int      `json:"file_count"`
		PackageCount   int      `json:"package_count"`
		EstimatedCalls int      `json:"estimated_calls"`
		Nodes          []string `json:"nodes"`
	} `json:"complex"`
}

func loadDelegationFixture(t *testing.T) delegationFixture {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", "delegation_tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture delegationFixture
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.TenantID == "" || fixture.RepositoryID == "" || len(fixture.Complex.Nodes) != 2 {
		t.Fatalf("invalid delegation fixture: %#v", fixture)
	}
	return fixture
}

func (f delegationFixture) simpleRequest() router.PPDRequest {
	return router.PPDRequest{FileCount: f.Simple.FileCount, PackageCount: f.Simple.PackageCount, EstimatedCalls: f.Simple.EstimatedCalls}
}

func (f delegationFixture) complexRequest(plannerCalls int) router.PPDRequest {
	return router.PPDRequest{FileCount: f.Complex.FileCount, PackageCount: f.Complex.PackageCount, EstimatedCalls: f.Complex.EstimatedCalls, PlannerCalls: plannerCalls}
}

type delegationPlanner struct {
	store   *plan.SQLStore
	fixture delegationFixture
	calls   int
}

func (p *delegationPlanner) Delegate(ctx context.Context) (plan.Plan, error) {
	p.calls++
	child := plan.Plan{
		TenantID: plan.TenantID(p.fixture.TenantID), RepositoryID: plan.RepositoryID(p.fixture.RepositoryID),
		TaskID: plan.TaskID(p.fixture.Complex.TaskID), ID: "planner-child-1", Generation: "one", State: plan.PlanDraft,
		Nodes:        []plan.Node{{ID: plan.NodeID(p.fixture.Complex.Nodes[0]), State: plan.NodePending}, {ID: plan.NodeID(p.fixture.Complex.Nodes[1]), State: plan.NodePending}},
		Dependencies: []plan.Dependency{{NodeID: plan.NodeID(p.fixture.Complex.Nodes[1]), DependsOn: plan.NodeID(p.fixture.Complex.Nodes[0])}},
	}
	if err := p.store.Create(ctx, child); err != nil {
		return plan.Plan{}, err
	}
	return child, nil
}

type delegationExecutor struct{ calls int }

func (e *delegationExecutor) Execute(context.Context, plan.NodeExecutionRequest) error {
	e.calls++
	return nil
}

func assertDelegationBudget(t *testing.T) {
	t.Helper()
	tracker := budget.NewTrackerWithUSDCap(0, 0, 100)
	id, err := tracker.Reserve("parent-task", "claude-sonnet-4-6", 10, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Reconcile(id, 5, 1); err != nil {
		t.Fatal(err)
	}
	if got := tracker.Cost("parent-task"); got.SpentMicrodollars != 30 || got.ReservedMicrodollars != 0 {
		t.Fatalf("parent total = %+v, want one reconciled child charge", got)
	}
	if _, err := tracker.Reserve("parent-task", "claude-haiku-4-5", 71, 0); !errors.Is(err, budget.ErrUSDBudgetExceeded) {
		t.Fatalf("subtree budget overrun error = %v, want ErrUSDBudgetExceeded", err)
	}
}

func assertDelegationRestartCancellationAndRecursion(t *testing.T, ctx context.Context, store *plan.SQLStore, policy *router.PPDPolicy, fixture delegationFixture) {
	t.Helper()
	child := plan.Plan{TenantID: plan.TenantID(fixture.TenantID), RepositoryID: plan.RepositoryID(fixture.RepositoryID), TaskID: "canceled-task", ID: "planner-child-canceled", Generation: "one", State: plan.PlanActive, Nodes: []plan.Node{{ID: "work", State: plan.NodePending}}}
	if err := store.Create(ctx, child); err != nil {
		t.Fatal(err)
	}
	scheduler := plan.NewScheduler(store, plan.SchedulerConfig{MaxAttempts: 1})
	claimed, err := scheduler.Claim(ctx, child, plan.ClaimRequest{AttemptID: "child-attempt", LeaseID: "child-lease", EventID: "child-claim", IdempotencyKey: "child-work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Cancel(ctx, child, claimed.ID, "child-lease", "child-cancel", "child-cancel"); err != nil {
		t.Fatal(err)
	}
	canceled, err := store.Load(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.State != plan.PlanCanceled || canceled.Nodes[0].State != plan.NodeCanceled {
		t.Fatalf("cancellation did not persist a terminal child state: %#v", canceled)
	}

	recursive := policy.Decide(fixture.complexRequest(1))
	if recursive.Action != router.PPDActionBypass || recursive.Reason != "planner_call_limit" {
		t.Fatalf("recursive delegation decision = %+v, want bounded bypass", recursive)
	}
}

func assertDelegationRestart(t *testing.T, ctx context.Context, fixture delegationFixture) {
	t.Helper()
	database := filepath.Join(t.TempDir(), "restart.db")
	store, err := plan.OpenSQLStore(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	child := plan.Plan{TenantID: plan.TenantID(fixture.TenantID), RepositoryID: plan.RepositoryID(fixture.RepositoryID), TaskID: "restart-task", ID: "planner-child-restart", Generation: "one", State: plan.PlanActive, Nodes: []plan.Node{{ID: "work", State: plan.NodePending}}}
	if err := store.Create(ctx, child); err != nil {
		t.Fatal(err)
	}
	scheduler := plan.NewScheduler(store, plan.SchedulerConfig{MaxAttempts: 2})
	claimed, err := scheduler.Claim(ctx, child, plan.ClaimRequest{AttemptID: "interrupted", LeaseID: "interrupted", EventID: "interrupted-claim", IdempotencyKey: "interrupted-work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Start(ctx, child, claimed.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = plan.OpenSQLStore(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := plan.NewScheduler(store, plan.SchedulerConfig{MaxAttempts: 2}).Retry(ctx, child, claimed.ID, "interrupted", "restart-retry", "restart-work"); err != nil {
		t.Fatal(err)
	}
	resumed, err := plan.NewController(store, &delegationExecutor{}, nil, nil, plan.ControllerConfig{Scheduler: plan.SchedulerConfig{MaxAttempts: 2}}).Run(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != plan.PlanCompleted || len(resumed.Attempts) != 2 {
		t.Fatalf("restarted child = %#v, want completed second attempt", resumed)
	}
}
