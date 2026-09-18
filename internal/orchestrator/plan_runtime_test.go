package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos/sdk/agent"
)

type completedPlanExecutor struct {
	calls int
}

func (e *completedPlanExecutor) Execute(_ context.Context, request plan.NodeExecutionRequest) (plan.NodeExecutionResult, error) {
	e.calls++
	return plan.NodeExecutionResult{NodeID: request.Node.ID, AttemptID: request.Attempt, Status: plan.NodeCompleted, Verification: plan.VerificationPassed}, nil
}

func TestExecutePlanPersistsResumesAndSynthesizesWithPrimary(t *testing.T) {
	ctx := context.Background()
	store, err := plan.OpenSQLStore(ctx, filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	primary := &executionTestProvider{name: "synthesis", modelID: "test"}
	executor := &completedPlanExecutor{}
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{"primary": newExecutionTestAgent("primary", primary)}, active: "primary", primary: "primary",
		routingConfig: &router.Config{PPD: router.PPDConfig{Mode: router.PPDModeEnabled}},
		capabilities:  RuntimeCapabilityManifest{Capabilities: []config.Capability{{Name: capabilityClosedLoopPPD}}},
		planStore:     store,
	}
	orch.planController = plan.NewController(store, executor, nil, nil, plan.ControllerConfig{})
	plannerJSON := []byte(`{"source_request_ref":"source","classifier_ref":"classifier","nodes":[{"id":"implement","depends_on":[],"scope":"internal/orchestrator/plan_runtime.go","context_refs":[],"risks":["integration"],"verification":"go test ./internal/orchestrator"}]}`)
	identity := PlanRuntimeIdentity{TenantID: "tenant", RepositoryID: "repo", TaskID: "task", PlanID: "plan", Generation: "one"}

	first, err := orch.ExecutePlan(ctx, plannerJSON, identity)
	if err != nil {
		t.Fatal(err)
	}
	second, err := orch.ExecutePlan(ctx, plannerJSON, identity)
	if err != nil {
		t.Fatal(err)
	}
	if first.Plan.State != plan.PlanCompleted || second.Plan.State != plan.PlanCompleted || executor.calls != 1 {
		t.Fatalf("plans = (%s, %s), node calls = %d", first.Plan.State, second.Plan.State, executor.calls)
	}
	if first.Synthesis == nil || first.Synthesis.AgentID != "primary" || len(primary.requests) != 2 {
		t.Fatalf("synthesis = %#v, primary calls = %d", first.Synthesis, len(primary.requests))
	}
	if message := userContent(primary.request(0)); strings.Contains(message, "integration") || !strings.Contains(message, "durable implementation plan completed") {
		t.Fatalf("synthesis prompt = %q", message)
	}
}

func TestExecutePlanRequiresExistingClosedLoopGate(t *testing.T) {
	orch := &Orchestrator{routingConfig: &router.Config{PPD: router.PPDConfig{Mode: router.PPDModeShadow}}}
	_, err := orch.ExecutePlan(context.Background(), []byte(`{}`), PlanRuntimeIdentity{})
	if err == nil || !strings.Contains(err.Error(), "capability is not enabled") {
		t.Fatalf("ExecutePlan() error = %v", err)
	}
}

func TestExecutePlanResumesOnlyIncompleteNodes(t *testing.T) {
	ctx := context.Background()
	store, err := plan.OpenSQLStore(ctx, filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	persisted := plan.Plan{
		TenantID: "tenant", RepositoryID: "repo", TaskID: "task", ID: "resume", Generation: "one", State: plan.PlanActive,
		Nodes: []plan.Node{
			{ID: "done", State: plan.NodeCompleted, Scope: "done.go", Risks: []string{"risk"}, Verification: "verify done"},
			{ID: "remaining", State: plan.NodePending, Scope: "remaining.go", Risks: []string{"risk"}, Verification: "verify remaining"},
		},
		Dependencies: []plan.Dependency{{NodeID: "remaining", DependsOn: "done"}},
		ContextRefs:  []plan.ContextRef{{ID: "source", NodeID: "done"}, {ID: "classifier", NodeID: "done"}},
	}
	if err := store.Create(ctx, persisted); err != nil {
		t.Fatal(err)
	}
	executor := &completedPlanExecutor{}
	primary := &executionTestProvider{name: "synthesis", modelID: "test"}
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{"primary": newExecutionTestAgent("primary", primary)}, active: "primary", primary: "primary",
		routingConfig: &router.Config{PPD: router.PPDConfig{Mode: router.PPDModeEnabled}},
		capabilities:  RuntimeCapabilityManifest{Capabilities: []config.Capability{{Name: capabilityClosedLoopPPD}}}, planStore: store,
	}
	orch.planController = plan.NewController(store, executor, nil, nil, plan.ControllerConfig{})
	plannerJSON := []byte(`{"source_request_ref":"source","classifier_ref":"classifier","nodes":[{"id":"done","depends_on":[],"scope":"done.go","context_refs":[],"risks":["risk"],"verification":"verify done"},{"id":"remaining","depends_on":["done"],"scope":"remaining.go","context_refs":[],"risks":["risk"],"verification":"verify remaining"}]}`)

	result, err := orch.ExecutePlan(ctx, plannerJSON, PlanRuntimeIdentity{TenantID: "tenant", RepositoryID: "repo", TaskID: "task", PlanID: "resume", Generation: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan.State != plan.PlanCompleted || executor.calls != 1 {
		t.Fatalf("resumed plan = %s, calls = %d", result.Plan.State, executor.calls)
	}
}
