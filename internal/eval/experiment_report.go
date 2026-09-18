package eval

import (
	"fmt"
	"math"
	"sort"
)

const ExperimentReportVersion = "chronos.eval.report.v1"

type RunMeasurement struct {
	TaskID   string      `json:"task_id"`
	RunID    string      `json:"run_id"`
	Ablation Ablation    `json:"ablation"`
	Outcome  TaskOutcome `json:"outcome"`
}

type RepeatedRunStats struct {
	TaskID                 string    `json:"task_id"`
	Ablation               Ablation  `json:"ablation"`
	Runs                   int       `json:"runs"`
	VerifiedSuccessRate    float64   `json:"verified_success_rate"`
	MeanTokens             float64   `json:"mean_tokens"`
	TokenSampleVariance    float64   `json:"token_sample_variance"`
	TokenConfidence95Lower float64   `json:"token_confidence_95_lower"`
	TokenConfidence95Upper float64   `json:"token_confidence_95_upper"`
	Failures               []Failure `json:"failures"`
}

type ExperimentReport struct {
	Version string             `json:"version"`
	Runs    []RunMeasurement   `json:"runs"`
	Stats   []RepeatedRunStats `json:"stats"`
}

// BuildExperimentReport retains every failed run and computes sample variance
// and a normal 95% confidence interval for repeated token measurements.
func BuildExperimentReport(runs []RunMeasurement) (ExperimentReport, error) {
	report := ExperimentReport{Version: ExperimentReportVersion, Runs: append([]RunMeasurement(nil), runs...)}
	type groupKey struct {
		task     string
		ablation Ablation
	}
	groups := make(map[groupKey][]RunMeasurement)
	for _, run := range runs {
		if run.TaskID == "" || run.RunID == "" || run.TaskID != run.Outcome.TaskID || run.RunID != run.Outcome.RunID {
			return ExperimentReport{}, fmt.Errorf("eval: run identity does not match its outcome")
		}
		if err := run.Outcome.Validate(); err != nil {
			return ExperimentReport{}, fmt.Errorf("eval: run %s: %w", run.RunID, err)
		}
		key := groupKey{task: run.TaskID, ablation: run.Ablation}
		groups[key] = append(groups[key], run)
	}
	keys := make([]groupKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return groupSortKey(keys[i].task, keys[i].ablation) < groupSortKey(keys[j].task, keys[j].ablation)
	})
	for _, key := range keys {
		group := groups[key]
		stats := RepeatedRunStats{TaskID: key.task, Ablation: key.ablation, Runs: len(group)}
		values := make([]float64, len(group))
		for i, run := range group {
			usage := run.Outcome.Usage()
			values[i] = float64(usage.InputTokens + usage.OutputTokens)
			stats.MeanTokens += values[i]
			if run.Outcome.Grader.Passed {
				stats.VerifiedSuccessRate++
			} else if run.Outcome.Failure != nil {
				stats.Failures = append(stats.Failures, *run.Outcome.Failure)
			}
		}
		stats.MeanTokens /= float64(len(group))
		stats.VerifiedSuccessRate /= float64(len(group))
		if len(group) > 1 {
			for _, value := range values {
				delta := value - stats.MeanTokens
				stats.TokenSampleVariance += delta * delta
			}
			stats.TokenSampleVariance /= float64(len(group) - 1)
		}
		margin := 1.96 * math.Sqrt(stats.TokenSampleVariance/float64(len(group)))
		stats.TokenConfidence95Lower = stats.MeanTokens - margin
		stats.TokenConfidence95Upper = stats.MeanTokens + margin
		report.Stats = append(report.Stats, stats)
	}
	return report, nil
}

func groupSortKey(task string, ablation Ablation) string {
	return fmt.Sprintf("%s:%t:%t:%t:%t:%t", task, ablation.Graph, ablation.Skills, ablation.Specialists, ablation.Repair, ablation.Worktrees)
}
