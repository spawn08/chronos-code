package eval

import (
	"math"
	"testing"
)

func TestBuildExperimentReportIncludesFailuresVarianceAndConfidenceInterval(t *testing.T) {
	first := validTaskOutcome()
	first.Calls = []Call{{ID: "one", Kind: CallModel, Usage: Usage{InputTokens: 80, OutputTokens: 20}}}
	second := validTaskOutcome()
	second.RunID = "run-2"
	second.Calls = []Call{{ID: "two", Kind: CallModel, Usage: Usage{InputTokens: 180, OutputTokens: 20}}}
	second.Grader.Passed = false
	second.Failure = &Failure{Class: FailureGrader, Message: "hidden check failed"}

	report, err := BuildExperimentReport([]RunMeasurement{
		{TaskID: first.TaskID, RunID: first.RunID, Outcome: first},
		{TaskID: second.TaskID, RunID: second.RunID, Outcome: second},
	})
	if err != nil {
		t.Fatalf("BuildExperimentReport: %v", err)
	}
	stats := report.Stats[0]
	if report.Version != ExperimentReportVersion || stats.Runs != 2 || stats.VerifiedSuccessRate != 0.5 || stats.MeanTokens != 150 || stats.TokenSampleVariance != 5000 || len(stats.Failures) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if math.Abs(stats.TokenConfidence95Lower-52) > 0.0001 || math.Abs(stats.TokenConfidence95Upper-248) > 0.0001 {
		t.Fatalf("confidence interval = [%f, %f]", stats.TokenConfidence95Lower, stats.TokenConfidence95Upper)
	}
}
