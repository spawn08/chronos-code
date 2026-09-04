package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

var ppdArms = []string{"ARM-A", "ARM-B", "ARM-C", "ARM-D"}
var ppdRepeats = []int{1, 2, 3}

type ppdTaskCorpus struct {
	Version      string         `yaml:"version"`
	CorpusID     string         `yaml:"corpus_id"`
	GraderAccess string         `yaml:"grader_access"`
	Controls     map[string]any `yaml:"controls"`
	Tasks        []struct {
		ID        string `yaml:"id"`
		Cohort    string `yaml:"cohort"`
		Objective string `yaml:"objective"`
		Grader    string `yaml:"grader"`
	} `yaml:"tasks"`
}

type ppdResults struct {
	Status        string `json:"status"`
	InvalidReason string `json:"invalid_reason"`
	Corpus        struct {
		ID        string         `json:"id"`
		TaskCount int            `json:"task_count"`
		Cohorts   map[string]int `json:"cohorts"`
	} `json:"corpus"`
	Runs []struct {
		TaskID        string   `json:"task_id"`
		Arms          []string `json:"arms"`
		Repeats       []int    `json:"repeats"`
		Status        string   `json:"status"`
		InvalidReason string   `json:"invalid_reason"`
	} `json:"runs"`
}

// ValidatePPDBenchmark validates the checked-in PPD benchmark registration
// without executing a model or changing the benchmark artifacts.
func ValidatePPDBenchmark(tasksPath, resultsPath string) error {
	tasksData, err := os.ReadFile(tasksPath)
	if err != nil {
		return fmt.Errorf("read tasks: %w", err)
	}
	resultsData, err := os.ReadFile(resultsPath)
	if err != nil {
		return fmt.Errorf("read results: %w", err)
	}

	var tasks ppdTaskCorpus
	yamlDecoder := yaml.NewDecoder(bytes.NewReader(tasksData))
	yamlDecoder.KnownFields(true)
	if err := yamlDecoder.Decode(&tasks); err != nil {
		return fmt.Errorf("decode tasks: %w", err)
	}
	var results ppdResults
	if err := json.Unmarshal(resultsData, &results); err != nil {
		return fmt.Errorf("decode results: %w", err)
	}

	if tasks.Version != "v1" || tasks.CorpusID == "" {
		return fmt.Errorf("tasks must define version v1 and a corpus ID")
	}
	if results.Corpus.ID != tasks.CorpusID {
		return fmt.Errorf("results corpus ID %q does not match tasks corpus ID %q", results.Corpus.ID, tasks.CorpusID)
	}
	if results.Corpus.TaskCount != len(tasks.Tasks) {
		return fmt.Errorf("results task count %d does not match %d registered tasks", results.Corpus.TaskCount, len(tasks.Tasks))
	}
	if results.Status != "invalid" || results.InvalidReason == "" {
		return fmt.Errorf("invalid benchmark results require an invalid reason")
	}

	taskCohorts := make(map[string]int, len(tasks.Tasks))
	taskIDs := make(map[string]struct{}, len(tasks.Tasks))
	for _, task := range tasks.Tasks {
		if task.ID == "" || task.Cohort == "" {
			return fmt.Errorf("tasks require IDs and cohorts")
		}
		if _, exists := taskIDs[task.ID]; exists {
			return fmt.Errorf("duplicate task ID %q", task.ID)
		}
		taskIDs[task.ID] = struct{}{}
		taskCohorts[task.Cohort]++
	}
	if !sameCounts(taskCohorts, results.Corpus.Cohorts) {
		return fmt.Errorf("results cohort counts do not match registered tasks")
	}

	if len(results.Runs) != len(tasks.Tasks) {
		return fmt.Errorf("results contain %d runs for %d registered tasks", len(results.Runs), len(tasks.Tasks))
	}
	runIDs := make(map[string]struct{}, len(results.Runs))
	for _, run := range results.Runs {
		if _, registered := taskIDs[run.TaskID]; !registered {
			return fmt.Errorf("result references unregistered task %q", run.TaskID)
		}
		if _, duplicate := runIDs[run.TaskID]; duplicate {
			return fmt.Errorf("duplicate result for task %q", run.TaskID)
		}
		runIDs[run.TaskID] = struct{}{}
		if !sameStrings(run.Arms, ppdArms) || !sameInts(run.Repeats, ppdRepeats) {
			return fmt.Errorf("task %q does not register the required four-arm, three-repeat matrix", run.TaskID)
		}
		if run.Status != "invalid" || run.InvalidReason == "" {
			return fmt.Errorf("invalid task run %q requires an invalid reason", run.TaskID)
		}
	}
	return nil
}

// PPDBenchmarkEvidenceStatus returns the evidence status recorded by a
// structurally valid benchmark registration. Registration validity does not
// imply that the benchmark was executed.
func PPDBenchmarkEvidenceStatus(resultsPath string) (string, string, error) {
	data, err := os.ReadFile(resultsPath)
	if err != nil {
		return "", "", fmt.Errorf("read results: %w", err)
	}
	var result struct {
		Status        string `json:"status"`
		InvalidReason string `json:"invalid_reason"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", "", fmt.Errorf("decode results status: %w", err)
	}
	return result.Status, result.InvalidReason, nil
}

func sameCounts(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]struct{}, len(left))
	for _, value := range left {
		seen[value] = struct{}{}
	}
	if len(seen) != len(right) {
		return false
	}
	for _, value := range right {
		if _, ok := seen[value]; !ok {
			return false
		}
	}
	return true
}

func sameInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[int]struct{}, len(left))
	for _, value := range left {
		seen[value] = struct{}{}
	}
	if len(seen) != len(right) {
		return false
	}
	for _, value := range right {
		if _, ok := seen[value]; !ok {
			return false
		}
	}
	return true
}
