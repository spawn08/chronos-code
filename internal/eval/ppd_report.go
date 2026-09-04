package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const ppdResultsVersion = "v1"

// PPDCompletedResults is the evidence accepted by the PPD efficacy gate.
// Unlike the registration placeholder, every run is one executed trial.
type PPDCompletedResults struct {
	Version      string    `json:"version"`
	ExperimentID string    `json:"experiment_id"`
	Status       string    `json:"status"`
	CompletedAt  time.Time `json:"completed_at"`
	Corpus       struct {
		ID       string `json:"id"`
		Revision string `json:"revision"`
	} `json:"corpus"`
	Model struct {
		Provider      string  `json:"provider"`
		Name          string  `json:"name"`
		Revision      string  `json:"revision"`
		Temperature   float64 `json:"temperature"`
		RealExecution bool    `json:"real_execution"`
	} `json:"model"`
	Environment struct {
		RepositoryRevision string `json:"repository_revision"`
		GraphWarmState     string `json:"graph_warm_state"`
		Permissions        string `json:"permissions"`
		ContextLimit       int    `json:"context_limit"`
		Timeout            string `json:"timeout"`
	} `json:"environment"`
	Hashes struct {
		Algorithm  string `json:"algorithm"`
		Corpus     string `json:"corpus"`
		Hypotheses string `json:"hypotheses"`
	} `json:"hashes"`
	Runs []PPDCompletedRun `json:"runs"`
}

// PPDCompletedRun contains measured evidence for one task, arm, and repeat.
type PPDCompletedRun struct {
	TaskID             string `json:"task_id"`
	Arm                string `json:"arm"`
	Repeat             int    `json:"repeat"`
	Status             string `json:"status"`
	Tokens             int    `json:"tokens"`
	ModelCalls         int    `json:"model_calls"`
	ToolCalls          int    `json:"tool_calls"`
	WallTimeMS         int64  `json:"wall_time_ms"`
	VerifiedSuccessful *bool  `json:"verified_successful"`
	MissedObligations  int    `json:"missed_obligations"`
}

// PPDGateBaseline pins the reproduction inputs that completed evidence must
// match before it may be used by the efficacy gate.
type PPDGateBaseline struct {
	Version            string `json:"version"`
	CorpusHash         string `json:"corpus_hash"`
	HypothesesHash     string `json:"hypotheses_hash"`
	ModelRevision      string `json:"model_revision"`
	RepositoryRevision string `json:"repository_revision"`
}

// BuildPPDReport validates completed real-execution evidence, applies the
// registered ARM-A versus ARM-D decision rule, and renders deterministic text.
func BuildPPDReport(tasksPath, hypothesesPath, resultsPath, baselinePath string) (string, error) {
	tasksData, err := os.ReadFile(tasksPath)
	if err != nil {
		return "", fmt.Errorf("read tasks: %w", err)
	}
	hypothesesData, err := os.ReadFile(hypothesesPath)
	if err != nil {
		return "", fmt.Errorf("read hypotheses: %w", err)
	}
	resultsData, err := os.ReadFile(resultsPath)
	if err != nil {
		return "", fmt.Errorf("read results: %w", err)
	}
	var envelope struct {
		Status        string `json:"status"`
		InvalidReason string `json:"invalid_reason"`
	}
	if err := json.Unmarshal(resultsData, &envelope); err != nil {
		return "", fmt.Errorf("decode results status: %w", err)
	}
	if envelope.Status != "completed" {
		if envelope.InvalidReason != "" {
			return "", fmt.Errorf("PPD gate: results are not completed evidence: %s", envelope.InvalidReason)
		}
		return "", fmt.Errorf("PPD gate: results status is %q, want completed", envelope.Status)
	}

	var tasks ppdTaskCorpus
	decoder := yaml.NewDecoder(bytes.NewReader(tasksData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&tasks); err != nil {
		return "", fmt.Errorf("decode tasks: %w", err)
	}
	registry, err := LoadHypothesisRegistry(hypothesesData)
	if err != nil {
		return "", err
	}
	var results PPDCompletedResults
	if err := decodeStrictJSON(resultsData, &results); err != nil {
		return "", fmt.Errorf("decode completed results: %w", err)
	}
	if err := validateCompletedPPD(results, tasks, tasksData, hypothesesData); err != nil {
		return "", err
	}
	baselineData, err := os.ReadFile(baselinePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("PPD gate: required baseline %s is missing", baselinePath)
		}
		return "", fmt.Errorf("PPD gate: read baseline: %w", err)
	}
	var baseline PPDGateBaseline
	if err := decodeStrictJSON(baselineData, &baseline); err != nil {
		return "", fmt.Errorf("PPD gate: decode baseline: %w", err)
	}
	if baseline.Version != ppdResultsVersion || baseline.CorpusHash != results.Hashes.Corpus || baseline.HypothesesHash != results.Hashes.Hypotheses || baseline.ModelRevision != results.Model.Revision || baseline.RepositoryRevision != results.Environment.RepositoryRevision {
		return "", fmt.Errorf("PPD gate: baseline is stale or does not match completed results")
	}

	taskCohorts := make(map[string]string, len(tasks.Tasks))
	for _, task := range tasks.Tasks {
		taskCohorts[task.ID] = task.Cohort
	}
	pairs := pairedPPDResults(results.Runs, taskCohorts, registry.Decision.RequiredCohorts)
	accepted, err := registry.Decision.Accepts(pairs)
	if err != nil {
		return "", fmt.Errorf("PPD gate: apply decision rule: %w", err)
	}
	report := renderPPDReport(results, accepted)
	if !accepted {
		return report, fmt.Errorf("PPD gate: completed results do not meet the registered efficacy thresholds")
	}
	return report, nil
}

func validateCompletedPPD(results PPDCompletedResults, tasks ppdTaskCorpus, tasksData, hypothesesData []byte) error {
	if results.Version != ppdResultsVersion || results.ExperimentID == "" || results.Status != "completed" || results.CompletedAt.IsZero() {
		return fmt.Errorf("PPD gate: completed results require version v1, experiment ID, status completed, and completed_at")
	}
	if tasks.Version != "v1" || tasks.CorpusID == "" || results.Corpus.ID != tasks.CorpusID || results.Corpus.Revision == "" || results.Corpus.Revision == "unexecuted" {
		return fmt.Errorf("PPD gate: completed results have invalid corpus metadata")
	}
	if !results.Model.RealExecution || results.Model.Provider == "" || results.Model.Name == "" || results.Model.Revision == "" {
		return fmt.Errorf("PPD gate: completed results require real model execution metadata")
	}
	if results.Environment.RepositoryRevision == "" || results.Environment.RepositoryRevision == "unexecuted" || results.Environment.GraphWarmState == "" || results.Environment.Permissions == "" || results.Environment.ContextLimit <= 0 || results.Environment.Timeout == "" || results.Environment.Timeout == "0s" {
		return fmt.Errorf("PPD gate: completed results require reproduction environment metadata")
	}
	if results.Hashes.Algorithm != "sha256" || results.Hashes.Corpus != sha256Hex(tasksData) || results.Hashes.Hypotheses != sha256Hex(hypothesesData) {
		return fmt.Errorf("PPD gate: results contain missing or stale corpus/hypothesis hashes")
	}

	if len(tasks.Tasks) == 0 {
		return fmt.Errorf("PPD gate: task corpus is empty")
	}
	taskIDs := make(map[string]struct{}, len(tasks.Tasks))
	for _, task := range tasks.Tasks {
		if task.ID == "" || task.Cohort == "" {
			return fmt.Errorf("PPD gate: tasks require IDs and cohorts")
		}
		if _, duplicate := taskIDs[task.ID]; duplicate {
			return fmt.Errorf("PPD gate: duplicate task ID %q", task.ID)
		}
		taskIDs[task.ID] = struct{}{}
	}
	wantRuns := len(tasks.Tasks) * len(ppdArms) * len(ppdRepeats)
	if len(results.Runs) != wantRuns {
		return fmt.Errorf("PPD gate: completed results contain %d trials, want %d", len(results.Runs), wantRuns)
	}
	seen := make(map[string]struct{}, wantRuns)
	for _, run := range results.Runs {
		if _, ok := taskIDs[run.TaskID]; !ok || !containsString(ppdArms, run.Arm) || !containsInt(ppdRepeats, run.Repeat) {
			return fmt.Errorf("PPD gate: trial references an unregistered task, arm, or repeat")
		}
		key := fmt.Sprintf("%s\x00%s\x00%d", run.TaskID, run.Arm, run.Repeat)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("PPD gate: duplicate trial for %s/%s/%d", run.TaskID, run.Arm, run.Repeat)
		}
		seen[key] = struct{}{}
		if run.Status != "completed" || run.VerifiedSuccessful == nil || run.Tokens <= 0 || run.ModelCalls <= 0 || run.ToolCalls < 0 || run.WallTimeMS <= 0 || run.MissedObligations < 0 {
			return fmt.Errorf("PPD gate: trial %s/%s/%d is invalid or unexecuted", run.TaskID, run.Arm, run.Repeat)
		}
	}
	return nil
}

func pairedPPDResults(runs []PPDCompletedRun, taskCohorts map[string]string, requiredCohorts []string) []PairedResult {
	required := stringsToSet(requiredCohorts)
	byKey := make(map[string]map[string]PPDCompletedRun)
	for _, run := range runs {
		cohort := taskCohorts[run.TaskID]
		if _, ok := required[cohort]; !ok {
			continue
		}
		key := fmt.Sprintf("%s\x00%d", run.TaskID, run.Repeat)
		if byKey[key] == nil {
			byKey[key] = make(map[string]PPDCompletedRun)
		}
		byKey[key][run.Arm] = run
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]PairedResult, 0, len(keys))
	for _, key := range keys {
		baseline, candidate := byKey[key]["ARM-A"], byKey[key]["ARM-D"]
		cohort := taskCohorts[baseline.TaskID]
		pairs = append(pairs, PairedResult{Cohort: cohort, BaselineSuccessful: *baseline.VerifiedSuccessful, CandidateSuccessful: *candidate.VerifiedSuccessful, BaselineTokens: float64(baseline.Tokens), CandidateTokens: float64(candidate.Tokens), BaselineCalls: float64(baseline.ModelCalls), CandidateCalls: float64(candidate.ModelCalls)})
	}
	return pairs
}

func renderPPDReport(results PPDCompletedResults, accepted bool) string {
	type totals struct {
		runs, tokens, calls, tools, successes, missed int
		wallTimeMS                                    int64
	}
	byArm := make(map[string]totals, len(ppdArms))
	for _, run := range results.Runs {
		t := byArm[run.Arm]
		t.runs++
		t.tokens += run.Tokens
		t.calls += run.ModelCalls
		t.tools += run.ToolCalls
		t.wallTimeMS += run.WallTimeMS
		t.missed += run.MissedObligations
		if *run.VerifiedSuccessful {
			t.successes++
		}
		byArm[run.Arm] = t
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# PPD Efficacy Report\n\n")
	fmt.Fprintf(&b, "- Experiment: %s\n- Completed: %s\n- Corpus: %s (%s, sha256:%s)\n- Hypotheses: sha256:%s\n- Model: %s/%s (%s, temperature %.2f)\n- Repository revision: %s\n- Environment: graph=%s, permissions=%s, context=%d, timeout=%s\n\n", results.ExperimentID, results.CompletedAt.UTC().Format(time.RFC3339), results.Corpus.ID, results.Corpus.Revision, results.Hashes.Corpus, results.Hashes.Hypotheses, results.Model.Provider, results.Model.Name, results.Model.Revision, results.Model.Temperature, results.Environment.RepositoryRevision, results.Environment.GraphWarmState, results.Environment.Permissions, results.Environment.ContextLimit, results.Environment.Timeout)
	fmt.Fprintf(&b, "| Arm | Runs | Tokens/accepted | Model calls/accepted | Tool calls | Wall time (ms) | Missed obligations | Verified success |\n|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, arm := range ppdArms {
		t := byArm[arm]
		tokensPerAccepted, callsPerAccepted := 0.0, 0.0
		if t.successes > 0 {
			tokensPerAccepted = float64(t.tokens) / float64(t.successes)
			callsPerAccepted = float64(t.calls) / float64(t.successes)
		}
		fmt.Fprintf(&b, "| %s | %d | %.1f | %.1f | %d | %d | %d | %.1f%% |\n", arm, t.runs, tokensPerAccepted, callsPerAccepted, t.tools, t.wallTimeMS, t.missed, float64(t.successes)*100/float64(t.runs))
	}
	decision := "FAIL"
	if accepted {
		decision = "PASS"
	}
	fmt.Fprintf(&b, "\nGate: %s\n", decision)
	return b.String()
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
