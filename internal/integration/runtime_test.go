package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	t.Run("legacy bypass", assertLegacyBypass)
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
		{"IC-002", runtimeEvidence{"internal/orchestrator/orchestrator.go", "func (h budgetHook) Before"}, []runtimeEvidence{{"internal/budget/budget.go", "EventModelCallBefore"}, {"internal/learning/telemetry_test.go", "TestTelemetryCorrelatesModelAndToolEvents"}, {"internal/orchestrator/execution_test.go", "TestExecuteUsesConcurrentRequestScopedProviders"}}},
		{"IC-003", runtimeEvidence{"internal/execution/ledger.go", "func (l *Ledger) Append"}, []runtimeEvidence{{"internal/verification/policy.go", "func Assess"}, {"internal/orchestrator/execution_test.go", "TestExecuteRejectsUnsupportedVerifiedCompletion"}}},
		{"IC-004", runtimeEvidence{"internal/eval/taskrunner.go", "TaskOutcome{"}, []runtimeEvidence{{"internal/eval/report.go", "func Summarize"}, {"internal/eval/gate.go", "func CheckRegression"}, {"internal/learning/learning_test.go", "ReplayEvidence"}}},
		{"IC-005", runtimeEvidence{"internal/memory/memory.go", "Fingerprint"}, []runtimeEvidence{{"internal/orchestrator/orchestrator.go", "store.ForContext(ctx)"}, {"internal/learning/learning_test.go", "TestStorePatternApprovalRetrievalAndRollback"}}},
		{"IC-006", runtimeEvidence{"internal/security/policy.go", "func ResolvePolicy"}, []runtimeEvidence{{"internal/integration/surfaces_test.go", "TestSurfacesTrustFailuresFailClosed"}, {"internal/config/capabilities_test.go", "TestCapabilityManifestValidateRejectsMissingRequiredCapability"}}},
		{"IC-007", runtimeEvidence{"internal/router/ppd.go", "func (p *PPDPolicy) Decide"}, []runtimeEvidence{{"internal/router/ppd_test.go", "TestPPDPolicy_ComplexTasksDelegate"}, {"internal/orchestrator/orchestrator.go", "ppdLoopDirective"}}},
	}
	if len(contracts) != 7 {
		t.Fatalf("contract evidence mappings = %d, want 7", len(contracts))
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
		case "external", "retired":
			if measure.Reason == "" {
				t.Errorf("%s %s acceptance condition has no reason", measure.ID, measure.Status)
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

func assertLegacyBypass(t *testing.T) {
	decision := router.NewPPDPolicy(router.PPDConfig{Version: "v1", Mode: router.PPDModeDisabled}, nil).Decide(router.PPDRequest{HighRisk: true})
	if decision.Action != router.PPDActionBypass || decision.Reason != "policy_disabled" {
		t.Fatalf("legacy non-PPD path = %#v, want policy-disabled bypass", decision)
	}
}
