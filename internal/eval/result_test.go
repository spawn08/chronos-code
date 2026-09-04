package eval

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func validTaskOutcome() TaskOutcome {
	changedAt := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	return TaskOutcome{
		Version:            TaskOutcomeVersion,
		TaskID:             "task-001",
		RunID:              "run-001",
		RepositoryRevision: "abc123",
		Model:              ModelSettings{Provider: "test", Name: "test-model"},
		Calls: []Call{
			{ID: "model-001", Kind: CallModel, Usage: Usage{InputTokens: 100, OutputTokens: 20, CostUSD: 0.01}},
			{ID: "tool-001", Kind: CallTool, Duration: time.Second},
		},
		Verification: []VerificationEvidence{
			{ID: "verify-001", Command: "go test ./...", Passed: true, ExecutedAt: changedAt.Add(time.Second)},
		},
		LatestChangeAt: changedAt,
		Grader:         GraderOutcome{Passed: true},
	}
}

func TestTaskOutcomeValidateSuccessfulRequiresFreshExecutableEvidence(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*TaskOutcome)
		wantErr string
	}{
		{
			name: "missing evidence",
			mutate: func(result *TaskOutcome) {
				result.Verification = nil
			},
			wantErr: "successful result requires executable verification evidence",
		},
		{
			name: "non-executable evidence",
			mutate: func(result *TaskOutcome) {
				result.Verification[0].Command = ""
			},
			wantErr: "verification evidence verify-001 is not executable",
		},
		{
			name: "stale evidence",
			mutate: func(result *TaskOutcome) {
				result.Verification[0].ExecutedAt = result.LatestChangeAt.Add(-time.Second)
			},
			wantErr: "verification evidence verify-001 predates the latest change",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validTaskOutcome()
			tt.mutate(&result)
			if err := result.Validate(); err == nil || err.Error() != tt.wantErr {
				t.Fatalf("Validate() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestTaskOutcomeUsageCountsUniqueCalls(t *testing.T) {
	result := validTaskOutcome()
	result.Calls[1].Usage = Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.02}
	result.Calls = append(result.Calls, Call{ID: "model-001", Kind: CallModel, Usage: Usage{InputTokens: 999}})

	if err := result.Validate(); err == nil || err.Error() != "duplicate call ID \"model-001\"" {
		t.Fatalf("Validate() error = %v, want duplicate call error", err)
	}

	result.Calls[2].ID = "model-002"
	usage := result.Usage()
	if usage.InputTokens != 1109 || usage.OutputTokens != 25 || usage.CostUSD != 0.03 {
		t.Errorf("Usage() = %+v, want totals from each unique call", usage)
	}
}

func TestTaskOutcomeJSONRoundTrip(t *testing.T) {
	want := validTaskOutcome()
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got TaskOutcome
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestTokensPerVerifiedSuccessIncludesFailedUsage(t *testing.T) {
	success := validTaskOutcome()
	failure := validTaskOutcome()
	failure.TaskID = "task-002"
	failure.RunID = "run-002"
	failure.Grader.Passed = false
	failure.Failure = &Failure{Class: FailureGrader, Message: "hidden test failed"}
	failure.Calls = []Call{{ID: "model-003", Kind: CallModel, Usage: Usage{InputTokens: 1000}}}

	metric, failed, err := TokensPerVerifiedSuccess([]TaskOutcome{success, failure})
	if err != nil {
		t.Fatalf("TokensPerVerifiedSuccess: %v", err)
	}
	if metric != 1120 {
		t.Errorf("metric = %v, want 1120", metric)
	}
	if !reflect.DeepEqual(failed, []string{"task-002"}) {
		t.Errorf("failed = %v, want [task-002]", failed)
	}
}
