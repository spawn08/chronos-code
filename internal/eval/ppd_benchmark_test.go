package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestValidatePPDBenchmark(t *testing.T) {
	tasksPath := "../../benchmark/ppd/tasks.yaml"
	resultsPath := "../../benchmark/ppd/results.json"
	if err := ValidatePPDBenchmark(tasksPath, resultsPath); err != nil {
		t.Fatalf("ValidatePPDBenchmark: %v", err)
	}
}

func TestValidatePPDBenchmarkRejectsMissingInvalidReason(t *testing.T) {
	tasksData, err := os.ReadFile("../../benchmark/ppd/tasks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resultsData, err := os.ReadFile("../../benchmark/ppd/results.json")
	if err != nil {
		t.Fatal(err)
	}
	resultsData = []byte(strings.Replace(string(resultsData), `"invalid_reason": "No real model available; all 12 required arm/repeat executions were not run."`, `"invalid_reason": ""`, 1))

	dir := t.TempDir()
	tasksPath := filepath.Join(dir, "tasks.yaml")
	resultsPath := filepath.Join(dir, "results.json")
	if err := os.WriteFile(tasksPath, tasksData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultsPath, resultsData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePPDBenchmark(tasksPath, resultsPath); err == nil || !strings.Contains(err.Error(), "invalid reason") {
		t.Fatalf("ValidatePPDBenchmark error = %v, want missing invalid reason", err)
	}
}

func TestValidatePPDBenchmarkRejectsIncompleteMatrix(t *testing.T) {
	tasksData, err := os.ReadFile("../../benchmark/ppd/tasks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resultsData, err := os.ReadFile("../../benchmark/ppd/results.json")
	if err != nil {
		t.Fatal(err)
	}
	resultsData = []byte(strings.Replace(string(resultsData), `"arms": ["ARM-A", "ARM-B", "ARM-C", "ARM-D"]`, `"arms": ["ARM-A", "ARM-B", "ARM-C"]`, 1))

	dir := t.TempDir()
	tasksPath := filepath.Join(dir, "tasks.yaml")
	resultsPath := filepath.Join(dir, "results.json")
	if err := os.WriteFile(tasksPath, tasksData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultsPath, resultsData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePPDBenchmark(tasksPath, resultsPath); err == nil || !strings.Contains(err.Error(), "four-arm, three-repeat matrix") {
		t.Fatalf("ValidatePPDBenchmark error = %v, want incomplete matrix", err)
	}
}

func TestBuildPPDReportRejectsRegistrationPlaceholder(t *testing.T) {
	_, err := BuildPPDReport("../../benchmark/ppd/tasks.yaml", "../../benchmark/ppd/hypotheses.yaml", "../../benchmark/ppd/results.json", filepath.Join(t.TempDir(), "baseline.json"))
	if err == nil || !strings.Contains(err.Error(), "not completed evidence") {
		t.Fatalf("BuildPPDReport error = %v, want uncompleted evidence rejection", err)
	}
}

func TestBuildPPDReportRequiresCurrentBaseline(t *testing.T) {
	tasksPath, hypothesesPath, resultsPath, baselinePath := writeCompletedPPDFixture(t)
	if err := os.Remove(baselinePath); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPPDReport(tasksPath, hypothesesPath, resultsPath, baselinePath); err == nil || !strings.Contains(err.Error(), "baseline") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("BuildPPDReport error = %v, want missing baseline", err)
	}

	_, _, resultsPath, baselinePath = writeCompletedPPDFixture(t)
	data, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"repository_revision":"revision-1"`, `"repository_revision":"stale"`, 1))
	if err := os.WriteFile(baselinePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPPDReport(tasksPath, hypothesesPath, resultsPath, baselinePath); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("BuildPPDReport error = %v, want stale baseline", err)
	}
}

func TestBuildPPDReportIsDeterministicForCompletedEvidence(t *testing.T) {
	tasksPath, hypothesesPath, resultsPath, baselinePath := writeCompletedPPDFixture(t)
	first, err := BuildPPDReport(tasksPath, hypothesesPath, resultsPath, baselinePath)
	if err != nil {
		t.Fatalf("BuildPPDReport: %v", err)
	}
	second, err := BuildPPDReport(tasksPath, hypothesesPath, resultsPath, baselinePath)
	if err != nil {
		t.Fatalf("BuildPPDReport second call: %v", err)
	}
	if first != second {
		t.Fatal("BuildPPDReport output is not deterministic")
	}
	for _, want := range []string{"Experiment: experiment-1", "Model: provider/model (model-1, temperature 0.00)", "Repository revision: revision-1", "| ARM-D |", "Gate: PASS"} {
		if !strings.Contains(first, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
}

func TestBuildPPDReportRejectsUnknownCompletedField(t *testing.T) {
	tasksPath, hypothesesPath, resultsPath, baselinePath := writeCompletedPPDFixture(t)
	data, err := os.ReadFile(resultsPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["unregistered_measurement"] = true
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultsPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPPDReport(tasksPath, hypothesesPath, resultsPath, baselinePath); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("BuildPPDReport error = %v, want strict schema rejection", err)
	}
}

func TestBuildPPDReportRejectsUnexecutedCompletedResult(t *testing.T) {
	tasksPath, hypothesesPath, resultsPath, baselinePath := writeCompletedPPDFixture(t)
	data, err := os.ReadFile(resultsPath)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"real_execution":true`, `"real_execution":false`, 1))
	if err := os.WriteFile(resultsPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPPDReport(tasksPath, hypothesesPath, resultsPath, baselinePath); err == nil || !strings.Contains(err.Error(), "real model execution") {
		t.Fatalf("BuildPPDReport error = %v, want unexecuted result rejection", err)
	}
}

func writeCompletedPPDFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	tasksData, err := os.ReadFile("../../benchmark/ppd/tasks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	hypothesesData, err := os.ReadFile("../../benchmark/ppd/hypotheses.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var tasks ppdTaskCorpus
	if err := yaml.Unmarshal(tasksData, &tasks); err != nil {
		t.Fatal(err)
	}

	results := PPDCompletedResults{Version: "v1", ExperimentID: "experiment-1", Status: "completed", CompletedAt: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	results.Corpus.ID, results.Corpus.Revision = tasks.CorpusID, "corpus-1"
	results.Model.Provider, results.Model.Name, results.Model.Revision, results.Model.RealExecution = "provider", "model", "model-1", true
	results.Environment.RepositoryRevision, results.Environment.GraphWarmState = "revision-1", "cold"
	results.Environment.Permissions, results.Environment.ContextLimit, results.Environment.Timeout = "deny", 32000, "10m"
	results.Hashes.Algorithm, results.Hashes.Corpus, results.Hashes.Hypotheses = "sha256", sha256Hex(tasksData), sha256Hex(hypothesesData)
	verified := true
	for _, task := range tasks.Tasks {
		for _, arm := range ppdArms {
			for _, repeat := range ppdRepeats {
				tokens, calls := 90, 9
				switch arm {
				case "ARM-A":
					tokens, calls = 100, 10
				case "ARM-D":
					tokens, calls = 80, 8
				}
				results.Runs = append(results.Runs, PPDCompletedRun{TaskID: task.ID, Arm: arm, Repeat: repeat, Status: "completed", Tokens: tokens, ModelCalls: calls, ToolCalls: 1, WallTimeMS: 100, VerifiedSuccessful: &verified})
			}
		}
	}
	baseline := PPDGateBaseline{Version: "v1", CorpusHash: results.Hashes.Corpus, HypothesesHash: results.Hashes.Hypotheses, ModelRevision: results.Model.Revision, RepositoryRevision: results.Environment.RepositoryRevision}

	dir := t.TempDir()
	tasksPath, hypothesesPath := filepath.Join(dir, "tasks.yaml"), filepath.Join(dir, "hypotheses.yaml")
	resultsPath, baselinePath := filepath.Join(dir, "results.json"), filepath.Join(dir, "baseline.json")
	write := func(path string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(tasksPath, tasksData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hypothesesPath, hypothesesData, 0o600); err != nil {
		t.Fatal(err)
	}
	write(resultsPath, results)
	write(baselinePath, baseline)
	return tasksPath, hypothesesPath, resultsPath, baselinePath
}
