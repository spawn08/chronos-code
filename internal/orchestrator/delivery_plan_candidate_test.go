package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	"github.com/spawn08/chronos/engine/tool"
)

// candidateGrantRunner fails a node unless it runs in a scratch worktree
// whose grant excludes checkout, network and external mutation.
type candidateGrantRunner struct {
	acceptedChainRunner
	calls int
}

func (r *candidateGrantRunner) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	r.calls++
	if !tool.IsScratchWorkspace(ctx) {
		return ExecutionResult{}, fmt.Errorf("candidate node is not bound to a scratch worktree")
	}
	if err := tool.RequireEffects(ctx, tool.EffectRead, tool.EffectScratchWrite, tool.EffectProcessExecution); err != nil {
		return ExecutionResult{}, fmt.Errorf("candidate grant lacks scratch effects: %w", err)
	}
	for _, effect := range []tool.Effect{tool.EffectDeliveryWrite, tool.EffectNetwork, tool.EffectExternalMutation} {
		if tool.RequireEffects(ctx, effect) == nil {
			return ExecutionResult{}, fmt.Errorf("candidate grant includes %s", effect)
		}
	}
	return r.acceptedChainRunner.Execute(ctx, request)
}

type candidatePlanFixture struct {
	repo       string
	manager    *worktree.Manager
	plans      *plan.SQLStore
	deliveries *execution.DeliveryStore
	orch       *Orchestrator
	runner     *candidateGrantRunner
	scope      execution.DeliveryScope
	plan       plan.Plan
}

func newCandidatePlanFixture(t *testing.T, policy string) *candidatePlanFixture {
	t.Helper()
	ctx := context.Background()
	f := &candidatePlanFixture{repo: initializePlanExecutorRepo(t), scope: execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}, runner: &candidateGrantRunner{}}
	var err error
	if f.manager, err = worktree.New(filepath.Join(t.TempDir(), "data"), nil); err != nil {
		t.Fatal(err)
	}
	if f.plans, err = plan.OpenSQLStore(ctx, filepath.Join(t.TempDir(), "plans.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.plans.Close() })
	if f.deliveries, err = execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.deliveries.Close() })
	if _, err := f.deliveries.AdmitRunnable(ctx, execution.Admission{
		Scope: f.scope, DeliveryID: "delivery", AdmissionKey: "request", PolicyReference: policy,
		Goal: execution.Goal{Statement: "add an API and a consumer", Actor: "operator"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"},
	}); err != nil {
		t.Fatal(err)
	}
	f.plan = plan.Plan{TenantID: "tenant", RepositoryID: "repo", TaskID: "delivery", ID: "delivery", Generation: "1", State: plan.PlanActive,
		Nodes:        []plan.Node{{ID: "a", State: plan.NodePending, Kind: plan.NodeImplement, Scope: "api.go", Verification: "verified by fixture"}, {ID: "b", State: plan.NodePending, Kind: plan.NodeImplement, Scope: "use.go", Verification: "verified by fixture"}},
		Dependencies: []plan.Dependency{{NodeID: "b", DependsOn: "a"}},
	}
	if err := f.plans.Create(ctx, f.plan); err != nil {
		t.Fatal(err)
	}
	// The candidate worker must not depend on the interactive closed-loop PPD
	// capability; the write executor keeps that gate.
	f.orch = &Orchestrator{
		planStore: f.plans, worktreeManager: f.manager, workspace: &workspace.Info{Root: f.repo}, planImplementationAgent: "coder",
	}

	f.orch.planController = plan.NewController(f.plans, &planNodeExecutor{runner: f.runner, worktrees: f.manager, repositoryRoot: f.repo, implementationAgent: "coder"}, nil, nil, plan.ControllerConfig{})
	return f
}

func (f *candidatePlanFixture) worker(t *testing.T, executor execution.Executor) *execution.Worker {
	t.Helper()
	worker, err := execution.NewWorker(f.deliveries, executor, execution.WorkerConfig{OwnerID: "plan-worker", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func planWorkerAuthorizer() authorization.Authorizer {
	return authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}
}

func TestCandidatePlanWorkerRetainsComposedArtifactsWithoutTouchingCheckout(t *testing.T) {
	ctx := context.Background()
	f := newCandidatePlanFixture(t, execution.CandidatePlanPolicyReference)
	candidate, err := NewCandidatePlanDeliveryExecutor(f.orch, planWorkerAuthorizer(), security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	routed, err := NewRoutedDeliveryExecutor(nil, candidate)
	if err != nil {
		t.Fatal(err)
	}
	worker := f.worker(t, routed)
	if !worker.CanRunCandidatePlan() || worker.CanRunPlan() || worker.CanRunCapped() || worker.CanRunCappedPlan() || worker.CanRunCheckpointedTeam() ||
		worker.PlanPolicyReference() != execution.CandidatePlanPolicyReference {
		t.Fatalf("candidate worker capabilities: plan=%v candidate=%v capped=%v policy=%q", worker.CanRunPlan(), worker.CanRunCandidatePlan(), worker.CanRunCapped(), worker.PlanPolicyReference())
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	completed, err := f.plans.Load(ctx, f.plan)
	if err != nil || completed.State != plan.PlanCompleted || len(completed.Artifacts) != 2 || !f.runner.observedAPI || f.runner.calls != 2 {
		t.Fatalf("candidate plan = %+v, observed API=%v, calls=%d, error=%v", completed, f.runner.observedAPI, f.runner.calls, err)
	}
	for _, artifact := range completed.Artifacts {
		if artifact.ReceiptID != "" {
			t.Fatalf("candidate node %q recorded an integration receipt %q", artifact.NodeID, artifact.ReceiptID)
		}
		patch, err := f.manager.LoadArtifact(ctx, artifact.ID)
		if err != nil || len(patch) == 0 {
			t.Fatalf("candidate artifact %q not retained: %v", artifact.ID, err)
		}
	}
	for _, name := range []string{"api.go", "use.go"} {
		if _, err := os.Stat(filepath.Join(f.repo, name)); !os.IsNotExist(err) {
			t.Fatalf("candidate plan wrote %s into the checkout: %v", name, err)
		}
	}
	status, err := exec.Command("git", "-C", f.repo, "status", "--porcelain").Output()
	if err != nil || strings.TrimSpace(string(status)) != "" {
		t.Fatalf("checkout changed by candidate plan: %q, error=%v", status, err)
	}
	delivery, err := f.deliveries.Load(ctx, f.scope, "delivery")
	if err != nil || delivery.State != execution.DeliveryWaitingDecision {
		t.Fatalf("candidate delivery = %+v, error=%v", delivery, err)
	}
	attempts, err := f.deliveries.Attempts(ctx, f.scope, "delivery")
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != execution.OutcomeWaiting {
		t.Fatalf("candidate attempts = %+v, error=%v", attempts, err)
	}
	var payload struct {
		Artifacts []plan.Artifact `json:"artifacts"`
	}
	if err := json.Unmarshal(attempts[0].Result, &payload); err != nil || len(payload.Artifacts) != 2 {
		t.Fatalf("parked candidate result = %+v, error=%v", payload, err)
	}
}

func TestPlanExecutorsRejectTheOtherAdmissionPolicy(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		policy    string
		candidate bool
	}{
		{name: "candidate worker refuses write plan", policy: execution.InternalPlanPolicyReference, candidate: true},
		{name: "write worker refuses candidate plan", policy: execution.CandidatePlanPolicyReference, candidate: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCandidatePlanFixture(t, tc.policy)
			var executor *PlanDeliveryExecutor
			var err error
			if tc.candidate {
				executor, err = NewCandidatePlanDeliveryExecutor(f.orch, planWorkerAuthorizer(), security.SandboxPolicy{})
			} else {
				if _, err := NewPlanDeliveryExecutor(f.orch, planWorkerAuthorizer(), security.SandboxPolicy{}); err == nil {
					t.Fatal("write plan executor started without the closed-loop PPD capability")
				}
				f.orch.routingConfig = &router.Config{PPD: router.PPDConfig{Mode: router.PPDModeEnabled}}
				f.orch.capabilities = RuntimeCapabilityManifest{Capabilities: []config.Capability{{Name: capabilityClosedLoopPPD}}}
				executor, err = NewPlanDeliveryExecutor(f.orch, planWorkerAuthorizer(), security.SandboxPolicy{})
			}
			if err != nil {
				t.Fatal(err)
			}
			routed, err := NewRoutedDeliveryExecutor(nil, executor)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.worker(t, routed).RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			loaded, err := f.plans.Load(ctx, f.plan)
			if err != nil || len(loaded.Attempts) != 0 || f.runner.calls != 0 {
				t.Fatalf("mismatched policy ran plan nodes: %+v, calls=%d, error=%v", loaded, f.runner.calls, err)
			}
			delivery, err := f.deliveries.Load(ctx, f.scope, "delivery")
			if err != nil || delivery.State != execution.DeliveryWaitingDecision {
				t.Fatalf("mismatched policy delivery = %+v, error=%v", delivery, err)
			}
		})
	}
}

func TestRoutedDeliveryExecutorParksKindsWithoutAnInstalledExecutor(t *testing.T) {
	ctx := context.Background()
	f := newCandidatePlanFixture(t, "admission-readonly-v1")
	candidate, err := NewCandidatePlanDeliveryExecutor(f.orch, planWorkerAuthorizer(), security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	routed, err := NewRoutedDeliveryExecutor(nil, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.worker(t, routed).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	delivery, err := f.deliveries.Load(ctx, f.scope, "delivery")
	if err != nil || delivery.State != execution.DeliveryWaitingDecision || f.runner.calls != 0 {
		t.Fatalf("read-only delivery without read-only executor = %+v, calls=%d, error=%v", delivery, f.runner.calls, err)
	}
	if _, err := NewRoutedDeliveryExecutor(nil, nil); err == nil {
		t.Fatal("routed executor accepted no executors")
	}
	f.orch.planImplementationAgent = ""
	if _, err := NewCandidatePlanDeliveryExecutor(f.orch, planWorkerAuthorizer(), security.SandboxPolicy{}); err == nil {
		t.Fatal("candidate plan executor accepted a runtime without an implementation agent")
	}
	f.orch.planImplementationAgent = "coder"
	f.orch.worktreeManager = nil
	if _, err := NewCandidatePlanDeliveryExecutor(f.orch, planWorkerAuthorizer(), security.SandboxPolicy{}); err == nil {
		t.Fatal("candidate plan executor accepted a runtime without patch retention")
	}
}

// TestCandidatePlanHandoffRunsDecomposedGenerationWithoutClosedLoopCapability
// covers the public path: strategist JSON is persisted as a draft generation,
// the delivery is made runnable, and the candidate worker activates and runs it.
func TestCandidatePlanHandoffRunsDecomposedGenerationWithoutClosedLoopCapability(t *testing.T) {
	ctx := context.Background()
	f := newCandidatePlanFixture(t, execution.CandidatePlanPolicyReference)
	// Replace the pre-created generation with one produced by the handoff.
	plans, err := plan.OpenSQLStore(ctx, filepath.Join(t.TempDir(), "handoff-plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer plans.Close()
	f.plans = plans
	f.orch.planStore = plans
	f.orch.planController = plan.NewController(plans, &planNodeExecutor{runner: f.runner, worktrees: f.manager, repositoryRoot: f.repo, implementationAgent: "coder"}, nil, nil, plan.ControllerConfig{})
	identity := PlanRuntimeIdentity{TenantID: "tenant", RepositoryID: "repo", TaskID: "delivery", PlanID: "delivery", Generation: "1"}
	proposal := []byte(`{"source_request_ref":"source","classifier_ref":"classifier","nodes":[` +
		`{"id":"a","kind":"implement","objective":"add api","depends_on":[],"scope":"api.go","context_refs":[],"expected_artifacts":["api.go"],"assumptions":[],"invalidation_triggers":[],"recovery_class":"replan","risks":["compatibility"],"verification":"go test ./..."},` +
		`{"id":"b","kind":"implement","objective":"use api","depends_on":["a"],"scope":"use.go","context_refs":[],"expected_artifacts":["use.go"],"assumptions":[],"invalidation_triggers":[],"recovery_class":"replan","risks":["compatibility"],"verification":"go test ./..."}]}`)
	if err := f.orch.PrepareDeliveryPlan(ctx, proposal, identity); err != nil {
		t.Fatalf("handoff without closed-loop capability: %v", err)
	}
	candidate, err := NewCandidatePlanDeliveryExecutor(f.orch, planWorkerAuthorizer(), security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	routed, err := NewRoutedDeliveryExecutor(nil, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.worker(t, routed).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ref := plan.Plan{TenantID: identity.TenantID, RepositoryID: identity.RepositoryID, TaskID: identity.TaskID, ID: identity.PlanID, Generation: identity.Generation}
	completed, err := plans.Load(ctx, ref)
	if err != nil || completed.State != plan.PlanCompleted || len(completed.Artifacts) != 2 || !f.runner.observedAPI {
		t.Fatalf("handoff plan = %+v, observed API=%v, error=%v", completed, f.runner.observedAPI, err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "api.go")); !os.IsNotExist(err) {
		t.Fatalf("handoff plan wrote the checkout: %v", err)
	}
	empty := &Orchestrator{planStore: plans, planController: f.orch.planController}
	if err := empty.PrepareDeliveryPlan(ctx, proposal, identity); err == nil {
		t.Fatal("handoff accepted a runtime with neither closed-loop capability nor candidate wiring")
	}
}
