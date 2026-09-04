package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/router"
	"gopkg.in/yaml.v3"
)

type runtimeEvidence struct {
	Path   string
	Marker string
}

type runtimeContract struct {
	ID        string
	Producer  runtimeEvidence
	Consumers []runtimeEvidence
}

type runtimeFixture struct {
	Version          string   `yaml:"version"`
	ID               string   `yaml:"id"`
	Kind             string   `yaml:"kind"`
	ChangedPaths     []string `yaml:"changed_paths"`
	RuntimeContracts []string `yaml:"runtime_contracts"`
	Verification     struct {
		Command             string `yaml:"command"`
		MustFollowLastWrite bool   `yaml:"must_follow_last_write"`
	} `yaml:"verification"`
	SuccessMeasures []struct {
		ID       string `yaml:"id"`
		Status   string `yaml:"status"`
		Artifact string `yaml:"artifact"`
		Marker   string `yaml:"marker"`
		Reason   string `yaml:"reason"`
	} `yaml:"success_measures"`
}

func TestRuntimeAcceptance(t *testing.T) {
	root := repositoryRoot(t)
	fixture := loadRuntimeFixture(t, root)
	assertRuntimeContractEvidence(t, root, fixture)
	assertCharterEvidence(t, root, fixture)
	t.Run("stale context and generation", assertCurrentRuntimeState)
	t.Run("backup restore and legacy bypass", assertRuntimeRollback)
}

func loadRuntimeFixture(t *testing.T, root string) runtimeFixture {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(root, "benchmark", "tasks", "verified-bugfix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture runtimeFixture
	if err := yaml.Unmarshal(contents, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != "v1" || fixture.ID == "" || fixture.Kind != "debug" || len(fixture.ChangedPaths) == 0 || fixture.Verification.Command == "" || !fixture.Verification.MustFollowLastWrite {
		t.Fatalf("invalid verified bugfix fixture: %#v", fixture)
	}
	return fixture
}

func assertRuntimeContractEvidence(t *testing.T, root string, fixture runtimeFixture) {
	t.Helper()
	contracts := []runtimeContract{
		{"IC-001", runtimeEvidence{"internal/orchestrator/orchestrator.go", "type ExecutionRequest struct"}, []runtimeEvidence{{"internal/integration/surfaces_test.go", "assertAdapterUsesExecute"}}},
		{"IC-002", runtimeEvidence{"internal/orchestrator/orchestrator.go", "func (h budgetHook) Before"}, []runtimeEvidence{{"internal/budget/budget.go", "EventModelCallBefore"}, {"internal/learning/telemetry_test.go", "TestTelemetryCorrelatesModelAndToolEvents"}, {"internal/orchestrator/orchestrator_test.go", "TestModelEscalationIsCapped"}}},
		{"IC-003", runtimeEvidence{"internal/execution/ledger.go", "func (l *Ledger) Append"}, []runtimeEvidence{{"internal/verification/policy.go", "func Assess"}, {"internal/orchestrator/execution_test.go", "TestExecuteRejectsUnsupportedVerifiedCompletion"}}},
		{"IC-004", runtimeEvidence{"internal/eval/taskrunner.go", "TaskOutcome{"}, []runtimeEvidence{{"internal/eval/report.go", "func Summarize"}, {"internal/eval/gate.go", "func CheckRegression"}, {"internal/learning/learning_test.go", "ReplayEvidence"}}},
		{"IC-005", runtimeEvidence{"internal/memory/memory.go", "Fingerprint"}, []runtimeEvidence{{"internal/orchestrator/orchestrator.go", "store.ForContext(ctx)"}, {"internal/learning/learning_test.go", "TestStorePatternApprovalRetrievalAndRollback"}}},
		{"IC-006", runtimeEvidence{"internal/security/security.go", "func LoadPolicy"}, []runtimeEvidence{{"internal/integration/surfaces_test.go", "TestSurfacesTrustFailuresFailClosed"}, {"internal/config/capabilities_test.go", "TestCapabilityManifestValidateRejectsMissingRequiredCapability"}}},
		{"IC-007", runtimeEvidence{"internal/plan/sqlstore.go", "type SQLStore struct"}, []runtimeEvidence{{"internal/plan/scheduler.go", "type Scheduler struct"}, {"internal/plan/controller.go", "type Controller struct"}, {"internal/plan/sqlstore_test.go", "TestSQLRoundTrip"}}},
		{"IC-008", runtimeEvidence{"internal/plan/scheduler.go", "func NewScheduler"}, []runtimeEvidence{{"internal/plan/controller.go", "NewScheduler(store"}, {"internal/integration/plan_recovery_test.go", "TestPlanRecovery"}}},
		{"IC-009", runtimeEvidence{"internal/plan/sqlstore.go", "func (s *SQLStore) Backup"}, []runtimeEvidence{{"internal/cli/plan_cmd_test.go", "TestPlanCommandInspectionJSON"}, {"scripts/ppd-db", "exec \"$binary\" plan"}}},
		{"IC-010", runtimeEvidence{"internal/router/ppd.go", "func (p *PPDPolicy) Decide"}, []runtimeEvidence{{"internal/integration/delegation_test.go", "TestDelegation"}, {"internal/plan/controller.go", "func (c *Controller) Run"}}},
	}
	if len(contracts) != 10 {
		t.Fatalf("contract evidence mappings = %d, want 10", len(contracts))
	}

	want := make(map[string]struct{}, len(fixture.RuntimeContracts))
	for _, id := range fixture.RuntimeContracts {
		if _, duplicate := want[id]; duplicate {
			t.Fatalf("duplicate runtime contract %s", id)
		}
		want[id] = struct{}{}
	}
	for index, contract := range contracts {
		expectedID := fmt.Sprintf("IC-%03d", index+1)
		if contract.ID != expectedID {
			t.Fatalf("contract evidence mapping %d = %s, want %s", index, contract.ID, expectedID)
		}
		if _, ok := want[contract.ID]; !ok {
			t.Errorf("%s has evidence but is absent from fixture", contract.ID)
			continue
		}
		delete(want, contract.ID)
		assertEvidence(t, root, contract.ID+" producer", contract.Producer)
		if len(contract.Consumers) == 0 {
			t.Errorf("%s has no consumer evidence", contract.ID)
		}
		for _, evidence := range contract.Consumers {
			assertEvidence(t, root, contract.ID+" consumer", evidence)
		}
	}
	for id := range want {
		t.Errorf("%s has no producer/consumer evidence mapping", id)
	}
}

func assertCharterEvidence(t *testing.T, root string, fixture runtimeFixture) {
	t.Helper()
	if len(fixture.SuccessMeasures) != 16 {
		t.Fatalf("success measure mappings = %d, want 16", len(fixture.SuccessMeasures))
	}
	seen := make(map[string]struct{}, len(fixture.SuccessMeasures))
	for index, measure := range fixture.SuccessMeasures {
		expectedID := fmt.Sprintf("SM-%02d", index+1)
		if measure.ID != expectedID {
			t.Errorf("success measure mapping %d = %s, want %s", index, measure.ID, expectedID)
		}
		if _, duplicate := seen[measure.ID]; duplicate {
			t.Errorf("duplicate success measure %s", measure.ID)
		}
		seen[measure.ID] = struct{}{}
		switch measure.Status {
		case "covered":
			assertEvidence(t, root, measure.ID, runtimeEvidence{measure.Artifact, measure.Marker})
		case "external":
			if measure.Reason == "" {
				t.Errorf("%s external acceptance condition has no reason", measure.ID)
			}
		default:
			t.Errorf("%s has unsupported status %q", measure.ID, measure.Status)
		}
	}
}

func assertEvidence(t *testing.T, root, label string, evidence runtimeEvidence) {
	t.Helper()
	if evidence.Path == "" || evidence.Marker == "" {
		t.Errorf("%s evidence is incomplete", label)
		return
	}
	contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(evidence.Path)))
	if err != nil {
		t.Errorf("%s evidence %s: %v", label, evidence.Path, err)
		return
	}
	if !strings.Contains(string(contents), evidence.Marker) {
		t.Errorf("%s evidence %s is missing marker %q", label, evidence.Path, evidence.Marker)
	}
}

func assertCurrentRuntimeState(t *testing.T) {
	ctx := context.Background()
	entries := []plan.ContextEntry{{ID: "request", Content: "original"}}
	current, err := plan.BuildRestartContext(entries, 1024, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.BuildRestartContext([]plan.ContextEntry{{ID: "request", Content: "changed"}}, 1024, current.Fingerprint); !errors.Is(err, plan.ErrStaleContextFingerprint) {
		t.Fatalf("changed restart context error = %v, want %v", err, plan.ErrStaleContextFingerprint)
	}

	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "generation.db"))
	source := runtimePlan("runtime-plan", "one", plan.PlanReplanning)
	if err := store.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	next, err := source.NewGeneration("two", []plan.Node{{ID: "work", State: plan.NodePending}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, next); err != nil {
		t.Fatal(err)
	}
	executor := &generationExecutor{}
	controller := plan.NewController(store, executor, nil, nil, plan.ControllerConfig{})
	stale, err := controller.Run(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if stale.State != plan.PlanReplanning || executor.generations != nil {
		t.Fatalf("superseded generation executed: state=%s calls=%v", stale.State, executor.generations)
	}
	completed, err := controller.Run(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != plan.PlanCompleted || fmt.Sprint(executor.generations) != "[two]" {
		t.Fatalf("current generation result=%s calls=%v", completed.State, executor.generations)
	}
}

func assertRuntimeRollback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := openRuntimeStore(t, filepath.Join(dir, "target.db"))
	legacy := runtimePlan("legacy-path", "one", plan.PlanDraft)
	if err := target.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(dir, "durable-backup.db")
	if _, err := target.Backup(ctx, plan.BackupRequest{Path: backupPath}); err != nil {
		t.Fatal(err)
	}

	sourcePath := filepath.Join(dir, "source.db")
	source := openRuntimeStore(t, sourcePath)
	replacement := runtimePlan("replacement", "two", plan.PlanDraft)
	if err := source.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	preRestorePath := filepath.Join(dir, "pre-restore.db")
	if _, err := target.Restore(ctx, plan.RestoreRequest{SourcePath: sourcePath, BackupPath: preRestorePath}); err != nil {
		t.Fatal(err)
	}
	assertStoredPlan(t, ctx, target, replacement.ID)
	assertStoredPlan(t, ctx, openRuntimeStore(t, backupPath), legacy.ID)
	assertStoredPlan(t, ctx, openRuntimeStore(t, preRestorePath), legacy.ID)

	decision := router.NewPPDPolicy(router.PPDConfig{Version: "v1", Mode: router.PPDModeDisabled}, nil).Decide(router.PPDRequest{HighRisk: true})
	if decision.Action != router.PPDActionBypass || decision.Reason != "policy_disabled" {
		t.Fatalf("legacy non-PPD path = %#v, want policy-disabled bypass", decision)
	}
}

type generationExecutor struct {
	generations []plan.GenerationID
}

func (e *generationExecutor) Execute(_ context.Context, request plan.NodeExecutionRequest) error {
	e.generations = append(e.generations, request.Plan.Generation)
	return nil
}

func runtimePlan(id plan.PlanID, generation plan.GenerationID, state plan.PlanState) plan.Plan {
	return plan.Plan{TenantID: "runtime-tenant", RepositoryID: "runtime-repository", TaskID: "verified-bugfix-001", ID: id, Generation: generation, State: state, Nodes: []plan.Node{{ID: "work", State: plan.NodePending}}}
}

func openRuntimeStore(t *testing.T, path string) *plan.SQLStore {
	t.Helper()
	store, err := plan.OpenSQLStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func assertStoredPlan(t *testing.T, ctx context.Context, store *plan.SQLStore, id plan.PlanID) {
	t.Helper()
	plans, err := store.List(ctx, plan.PlanScope{TenantID: "runtime-tenant", RepositoryID: "runtime-repository"})
	if err != nil || len(plans) != 1 || plans[0].PlanID != id {
		t.Fatalf("stored plans = %#v, error = %v; want %s", plans, err, id)
	}
}
