package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/workspace"
	"github.com/spawn08/chronos-code/internal/worktree"
)

func TestPrepareDeliveryPlanGenerationIsRetryableAndRejectsChangedProposal(t *testing.T) {
	ctx := context.Background()
	store, err := plan.OpenSQLStore(ctx, filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	orch := &Orchestrator{planStore: store, routingConfig: &router.Config{PPD: router.PPDConfig{Mode: router.PPDModeEnabled}}, capabilities: RuntimeCapabilityManifest{Capabilities: []config.Capability{{Name: capabilityClosedLoopPPD}}}}
	orch.planController = plan.NewController(store, nil, nil, nil, plan.ControllerConfig{})
	identity := PlanRuntimeIdentity{TenantID: "tenant", RepositoryID: "repo", TaskID: "delivery", PlanID: "delivery", Generation: "1"}
	proposal := []byte(`{"source_request_ref":"source","classifier_ref":"classifier","nodes":[{"id":"a","kind":"implement","objective":"add api","depends_on":[],"scope":"api.go","context_refs":[],"expected_artifacts":["api.go"],"assumptions":[],"invalidation_triggers":[],"recovery_class":"replan","risks":["compatibility"],"verification":"go test ./..."}]}`)
	if err := orch.PrepareDeliveryPlan(ctx, proposal, identity); err != nil {
		t.Fatal(err)
	}
	if err := orch.PrepareDeliveryPlan(ctx, proposal, identity); err != nil {
		t.Fatalf("retry same proposal: %v", err)
	}
	changed := []byte(strings.Replace(string(proposal), "add api", "delete api", 1))
	if err := orch.PrepareDeliveryPlan(ctx, changed, identity); !errors.Is(err, ErrPlanHandoffConflict) {
		t.Fatalf("changed generation became runnable: %v", err)
	}
	loaded, err := store.Load(ctx, plan.Plan{TenantID: identity.TenantID, RepositoryID: identity.RepositoryID, TaskID: identity.TaskID, ID: identity.PlanID, Generation: identity.Generation})
	if err != nil || loaded.State != plan.PlanDraft || len(loaded.Nodes) != 1 || loaded.Nodes[0].Objective != "add api" {
		t.Fatalf("persisted generation = %+v, error = %v", loaded, err)
	}
}

func TestVerifiedIntegrationReceiptReconcilesExpiredPlanNodeWithoutReapplying(t *testing.T) {
	ctx := context.Background()
	repo := initializePlanExecutorRepo(t)
	manager, err := worktree.New(filepath.Join(t.TempDir(), "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := plan.OpenSQLStore(ctx, filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer plans.Close()
	deliveries, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer deliveries.Close()
	identity := PlanRuntimeIdentity{TenantID: "tenant", RepositoryID: "repo", TaskID: "delivery", PlanID: "delivery", Generation: "1"}
	p := plan.Plan{TenantID: identity.TenantID, RepositoryID: identity.RepositoryID, TaskID: identity.TaskID, ID: identity.PlanID, Generation: identity.Generation, State: plan.PlanActive,
		Nodes: []plan.Node{{ID: "a", State: plan.NodePending}, {ID: "b", State: plan.NodePending}}, Dependencies: []plan.Dependency{{NodeID: "b", DependsOn: "a"}}}
	if err := plans.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	scheduler := plan.NewScheduler(plans, plan.SchedulerConfig{LeaseDuration: time.Minute, Now: func() time.Time { return now }})
	claim := plan.ClaimRequest{AttemptID: "controller-a-1", LeaseID: "controller-a-1", EventID: "controller-a-1", IdempotencyKey: "controller-a-1"}
	if _, err := scheduler.Claim(ctx, p, claim); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Start(ctx, p, "a", claim.LeaseID); err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Create(ctx, repo, worktree.CreateOptions{TaskID: "delivery", AttemptID: string(claim.AttemptID), DirtyPolicy: worktree.DirtyReject})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(handle.Manifest.WorktreePath, "api.go")
	if err := os.WriteFile(path, []byte("package fixture\nfunc API() int { return 42 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := []worktree.Check{{Name: "plan-node-verification", Passed: true}}
	collected, err := manager.Collect(ctx, handle, checks)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := manager.IntegrateVerified(ctx, handle, []string{"api.go"}, checks, collected.FinalHash)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := scheduler.Ready(ctx, p); err != nil {
		t.Fatal(err)
	}
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := deliveries.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", PolicyReference: execution.InternalPlanPolicyReference,
		Goal: execution.Goal{Statement: "build api", Actor: "operator"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := deliveries.Claim(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parked, err := deliveries.Finalize(ctx, lease, execution.Outcome{Kind: execution.OutcomeWaiting, WaitState: execution.DeliveryWaitingDecision, Signal: "plan-review"})
	if err != nil {
		t.Fatal(err)
	}
	orch := &Orchestrator{planStore: plans, worktreeManager: manager, workspace: &workspace.Info{Root: repo}}
	authorized := authorization.WithRequest(ctx, authorization.Request{PrincipalID: "operator", TenantID: "tenant", RepositoryID: "repo", Action: "plan.reconcile"})
	if err := orch.ReconcilePlanReceipt(ctx, deliveries, identity, "a", claim.AttemptID, receipt.ReceiptID, parked.Version); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("unauthorized recovery: %v", err)
	}
	if err := orch.ReconcilePlanReceipt(authorized, deliveries, identity, "a", "wrong-attempt", receipt.ReceiptID, parked.Version); err == nil {
		t.Fatal("unrelated plan attempt accepted receipt")
	}
	if err := orch.ReconcilePlanReceipt(authorized, deliveries, identity, "a", claim.AttemptID, receipt.ReceiptID, parked.Version); err != nil {
		t.Fatal(err)
	}
	if err := orch.ReconcilePlanReceipt(authorized, deliveries, identity, "a", claim.AttemptID, receipt.ReceiptID, parked.Version); err != nil {
		t.Fatalf("same receipt after crash/retry: %v", err)
	}
	if err := orch.ReconcilePlanReceipt(authorized, deliveries, identity, "a", claim.AttemptID, receipt.ReceiptID, parked.Version+1); !errors.Is(err, execution.ErrStaleDeliveryVersion) {
		t.Fatalf("wrong delivery version accepted a receipt: %v", err)
	}
	loaded, err := plans.Load(ctx, p)
	if err != nil || loaded.State != plan.PlanActive || loaded.Nodes[0].State != plan.NodeCompleted || loaded.Nodes[1].State != plan.NodeReady || len(loaded.Artifacts) != 1 || loaded.Artifacts[0].ID != receipt.ArtifactID {
		t.Fatalf("reconciled plan = %+v, error = %v", loaded, err)
	}
	content, err := os.ReadFile(filepath.Join(repo, "api.go"))
	if err != nil || string(content) != "package fixture\nfunc API() int { return 42 }\n" {
		t.Fatalf("patch applied twice or changed: %q, error = %v", content, err)
	}
}

func TestAdmittedPlanWorkerStartExecutesPersistedNodesAndParksForAcceptance(t *testing.T) {
	ctx := context.Background()
	repo := initializePlanExecutorRepo(t)
	manager, err := worktree.New(filepath.Join(t.TempDir(), "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := plan.OpenSQLStore(ctx, filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer plans.Close()
	deliveries, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer deliveries.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := deliveries.AdmitRunnable(ctx, execution.Admission{
		Scope: scope, DeliveryID: "delivery", AdmissionKey: "request", PolicyReference: execution.InternalPlanPolicyReference,
		Goal: execution.Goal{Statement: "add an API and a consumer", Actor: "operator"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"},
	}); err != nil {
		t.Fatal(err)
	}
	p := plan.Plan{TenantID: "tenant", RepositoryID: "repo", TaskID: "delivery", ID: "delivery", Generation: "1", State: plan.PlanActive,
		Nodes:        []plan.Node{{ID: "a", State: plan.NodePending, Kind: plan.NodeImplement, Scope: "api.go", Verification: "verified by fixture"}, {ID: "b", State: plan.NodePending, Kind: plan.NodeImplement, Scope: "use.go", Verification: "verified by fixture"}},
		Dependencies: []plan.Dependency{{NodeID: "b", DependsOn: "a"}},
	}
	if err := plans.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	runner := &acceptedChainRunner{}
	orch := &Orchestrator{
		planStore: plans, worktreeManager: manager, workspace: &workspace.Info{Root: repo},
		routingConfig: &router.Config{PPD: router.PPDConfig{Mode: router.PPDModeEnabled}},
		capabilities:  RuntimeCapabilityManifest{Capabilities: []config.Capability{{Name: capabilityClosedLoopPPD}}},
	}
	orch.planController = plan.NewController(plans, &planNodeExecutor{runner: runner, worktrees: manager, repositoryRoot: repo, implementationAgent: "coder"}, nil, nil, plan.ControllerConfig{})
	executor, err := NewPlanDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(deliveries, executor, execution.WorkerConfig{OwnerID: "plan-worker", Concurrency: 1, LeaseDuration: 5 * time.Second, HeartbeatEvery: 100 * time.Millisecond, PollEvery: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := worker.Start(workerCtx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		loaded, err := deliveries.Load(ctx, scope, "delivery")
		if err != nil {
			t.Fatal(err)
		}
		if loaded.State == execution.DeliveryWaitingDecision {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	if err := worker.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	completed, err := plans.Load(ctx, p)
	if err != nil || completed.State != plan.PlanCompleted || len(completed.Artifacts) != 2 || !runner.observedAPI {
		t.Fatalf("worker plan = %+v, observed API=%v, error=%v, worker error=%v", completed, runner.observedAPI, err, worker.LastError())
	}
	attempts, err := deliveries.Attempts(ctx, scope, "delivery")
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != execution.OutcomeWaiting {
		t.Fatalf("worker attempts = %+v, error=%v", attempts, err)
	}
	var payload struct {
		Artifacts []plan.Artifact `json:"artifacts"`
	}
	if err := json.Unmarshal(attempts[0].Result, &payload); err != nil || len(payload.Artifacts) != 2 {
		t.Fatalf("parked plan result = %+v, error=%v", payload, err)
	}
}

func TestReplacementPlanWorkerExecutesOnlyIncompleteAcceptedDependency(t *testing.T) {
	ctx := context.Background()
	repo := initializePlanExecutorRepo(t)
	manager, err := worktree.New(filepath.Join(t.TempDir(), "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err := manager.Create(ctx, repo, worktree.CreateOptions{TaskID: "delivery", AttemptID: "accepted-a", DirtyPolicy: worktree.DirtyReject})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(predecessor.Manifest.WorktreePath, "api.go"), []byte("package fixture\nfunc API() int { return 42 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	accepted, err := manager.Integrate(ctx, predecessor, []string{"api.go"})
	if err != nil {
		t.Fatal(err)
	}
	plans, err := plan.OpenSQLStore(ctx, filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer plans.Close()
	p := plan.Plan{TenantID: "tenant", RepositoryID: "repo", TaskID: "delivery", ID: "delivery", Generation: "1", State: plan.PlanActive,
		Nodes:        []plan.Node{{ID: "a", State: plan.NodeCompleted, Kind: plan.NodeImplement, Scope: "api.go", Verification: "verified"}, {ID: "b", State: plan.NodePending, Kind: plan.NodeImplement, Scope: "use.go", Verification: "verified"}},
		Dependencies: []plan.Dependency{{NodeID: "b", DependsOn: "a"}},
		Artifacts:    []plan.Artifact{{NodeID: "a", ID: accepted.ArtifactID, ReceiptID: accepted.ReceiptID}},
	}
	if err := plans.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	clock := &deliveryClock{}
	clock.timestamp.Store(time.Now().Add(time.Second).UnixNano())
	path := filepath.Join(t.TempDir(), "deliveries.db")
	deliveries, err := execution.OpenDeliveryStoreWithClock(ctx, path, clock)
	if err != nil {
		t.Fatal(err)
	}
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := deliveries.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "request", PolicyReference: execution.InternalPlanPolicyReference, Goal: execution.Goal{Statement: "finish the consumer", Actor: "operator"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	stale, err := deliveries.Claim(ctx, "vanished", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := deliveries.Close(); err != nil {
		t.Fatal(err)
	}
	clock.timestamp.Add(int64(time.Minute + time.Second))
	deliveries, err = execution.OpenDeliveryStoreWithClock(ctx, path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer deliveries.Close()
	runner := &acceptedChainRunner{}
	orch := &Orchestrator{planStore: plans, worktreeManager: manager, workspace: &workspace.Info{Root: repo}, routingConfig: &router.Config{PPD: router.PPDConfig{Mode: router.PPDModeEnabled}}, capabilities: RuntimeCapabilityManifest{Capabilities: []config.Capability{{Name: capabilityClosedLoopPPD}}}}
	orch.planController = plan.NewController(plans, &planNodeExecutor{runner: runner, worktrees: manager, repositoryRoot: repo, implementationAgent: "coder"}, nil, nil, plan.ControllerConfig{})
	executor, err := NewPlanDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(deliveries, executor, execution.WorkerConfig{OwnerID: "replacement", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, err := plans.Load(ctx, p)
	if err != nil || loaded.State != plan.PlanCompleted || len(loaded.Attempts) != 1 || loaded.Attempts[0].NodeID != "b" || !runner.observedAPI {
		t.Fatalf("resumed plan = %+v, observed accepted API=%v, error=%v", loaded, runner.observedAPI, err)
	}
	if err := deliveries.PublishResult(ctx, stale, json.RawMessage(`{"unsafe":true}`)); !errors.Is(err, execution.ErrStaleLease) {
		t.Fatalf("stale plan worker published output: %v", err)
	}
}

type cancelAfterPlanNode struct {
	store *execution.DeliveryStore
	scope execution.DeliveryScope
	calls []plan.NodeID
}

func (e *cancelAfterPlanNode) Execute(ctx context.Context, request plan.NodeExecutionRequest) (plan.NodeExecutionResult, error) {
	e.calls = append(e.calls, request.Node.ID)
	if request.Node.ID == "a" {
		if err := e.store.Cancel(ctx, e.scope, execution.DeliveryID(request.Plan.TaskID)); err != nil {
			return plan.NodeExecutionResult{}, err
		}
	}
	return plan.NodeExecutionResult{NodeID: request.Node.ID, AttemptID: request.Attempt, Status: plan.NodeCompleted, Verification: plan.VerificationPassed}, nil
}

func TestExplicitDeliveryCancelStopsPlanNodeAdmission(t *testing.T) {
	ctx := context.Background()
	deliveries, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer deliveries.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := deliveries.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "request", PolicyReference: execution.InternalPlanPolicyReference, Goal: execution.Goal{Statement: "finish two nodes", Actor: "operator"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	plans, err := plan.OpenSQLStore(ctx, filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer plans.Close()
	p := plan.Plan{TenantID: "tenant", RepositoryID: "repo", TaskID: "delivery", ID: "delivery", Generation: "1", State: plan.PlanActive,
		Nodes: []plan.Node{{ID: "a", State: plan.NodePending}, {ID: "b", State: plan.NodePending}}, Dependencies: []plan.Dependency{{NodeID: "b", DependsOn: "a"}}}
	if err := plans.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	nodeExecutor := &cancelAfterPlanNode{store: deliveries, scope: scope}
	orch := &Orchestrator{planStore: plans, workspace: &workspace.Info{Root: t.TempDir()}, routingConfig: &router.Config{PPD: router.PPDConfig{Mode: router.PPDModeEnabled}}, capabilities: RuntimeCapabilityManifest{Capabilities: []config.Capability{{Name: capabilityClosedLoopPPD}}}}
	orch.planController = plan.NewController(plans, nodeExecutor, nil, nil, plan.ControllerConfig{})
	executor, err := NewPlanDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(deliveries, executor, execution.WorkerConfig{OwnerID: "worker", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); !errors.Is(err, execution.ErrStaleLease) {
		t.Fatalf("canceled delivery worker finalized a plan: %v", err)
	}
	loaded, err := plans.Load(ctx, p)
	if err != nil || len(loaded.Attempts) != 1 || loaded.Attempts[0].NodeID != "a" || len(nodeExecutor.calls) != 1 {
		t.Fatalf("post-cancel plan = %+v, calls=%v, error=%v", loaded, nodeExecutor.calls, err)
	}
	delivery, err := deliveries.Load(ctx, scope, "delivery")
	if err != nil || delivery.State != execution.DeliveryCancelRequested {
		t.Fatalf("explicit cancellation = %+v, error=%v", delivery, err)
	}
}
