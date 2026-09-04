package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeTaskAdapter struct {
	run func(context.Context, TaskExecution) (TaskExecutionResult, error)
}

func (f fakeTaskAdapter) Run(ctx context.Context, execution TaskExecution) (TaskExecutionResult, error) {
	return f.run(ctx, execution)
}

func TestTaskRunnerIsolatesRepeatedFixtureRuns(t *testing.T) {
	fixture, revision := taskRunnerFixture(t)
	runner := TaskRunner{Adapter: fakeTaskAdapter{run: func(_ context.Context, execution TaskExecution) (TaskExecutionResult, error) {
		if err := os.WriteFile(filepath.Join(execution.Workspace, "message.txt"), []byte("changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return TaskExecutionResult{Grader: GraderOutcome{Passed: false}}, nil
	}}}
	request := TaskRun{TaskID: "task-001", Fixture: fixture, Revision: revision, Model: ModelSettings{Provider: "test", Name: "fake"}}

	first, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if first.Patch != second.Patch || !strings.Contains(first.Patch, "+changed") {
		t.Fatalf("patches = %q, %q; want identical changed patch", first.Patch, second.Patch)
	}
	data, err := os.ReadFile(filepath.Join(fixture, "message.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original\n" {
		t.Fatalf("fixture content = %q, want original", data)
	}
}

func TestTaskRunnerRecordsOutcomeAndArtifacts(t *testing.T) {
	fixture, revision := taskRunnerFixture(t)
	changedAt := time.Now()
	runner := TaskRunner{Adapter: fakeTaskAdapter{run: func(_ context.Context, execution TaskExecution) (TaskExecutionResult, error) {
		if err := os.WriteFile(filepath.Join(execution.Workspace, "message.txt"), []byte("updated\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return TaskExecutionResult{
			Calls:          []Call{{ID: "call-001", Kind: CallModel, Usage: Usage{InputTokens: 3}}},
			Events:         []TaskEvent{{ID: "event-001", Name: "completed"}},
			ExitCode:       0,
			Verification:   []VerificationEvidence{{ID: "verify-001", Command: "go test ./...", Passed: true, ExecutedAt: changedAt.Add(time.Nanosecond)}},
			LatestChangeAt: changedAt,
			Grader:         GraderOutcome{Passed: true},
		}, nil
	}}}

	result, err := runner.Run(context.Background(), TaskRun{TaskID: "task-001", RunID: "run-001", Fixture: fixture, Revision: revision, Model: ModelSettings{Provider: "test", Name: "fake"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := result.Outcome.Validate(); err != nil {
		t.Fatalf("outcome validation: %v", err)
	}
	if result.Outcome.TaskID != "task-001" || result.Outcome.RunID != "run-001" || result.Outcome.RepositoryRevision != revision || result.Outcome.Model != (ModelSettings{Provider: "test", Name: "fake"}) || len(result.Outcome.Calls) != 1 || len(result.Outcome.Verification) != 1 || !result.Outcome.LatestChangeAt.Equal(changedAt) || !result.Outcome.Grader.Passed {
		t.Fatalf("outcome identity = %+v", result.Outcome)
	}
	if len(result.Events) != 1 || result.ExitCode != 0 || !strings.Contains(result.Patch, "+updated") {
		t.Fatalf("artifacts = %+v", result)
	}
}

func TestTaskRunnerTimeoutAndCancellationPreserveArtifacts(t *testing.T) {
	fixture, revision := taskRunnerFixture(t)
	for _, tt := range []struct {
		name    string
		timeout time.Duration
		cancel  bool
	}{
		{name: "timeout", timeout: time.Millisecond},
		{name: "cancellation", cancel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := TaskRunner{Adapter: fakeTaskAdapter{run: func(ctx context.Context, execution TaskExecution) (TaskExecutionResult, error) {
				if err := os.WriteFile(filepath.Join(execution.Workspace, "message.txt"), []byte("partial\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if tt.cancel {
					cancel()
				}
				<-ctx.Done()
				return TaskExecutionResult{Events: []TaskEvent{{ID: "event-001", Name: "cancelled"}}}, ctx.Err()
			}}}
			result, err := runner.Run(ctx, TaskRun{TaskID: "task-001", Fixture: fixture, Revision: revision, Model: ModelSettings{Provider: "test", Name: "fake"}, Timeout: tt.timeout})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Outcome.Failure == nil || result.Outcome.Failure.Class != FailureExecution || !strings.Contains(result.Patch, "+partial") || len(result.Events) != 1 {
				t.Fatalf("result = %+v, want execution failure with artifacts", result)
			}
		})
	}
}

func taskRunnerFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "message.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "fixture"}} {
		if output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	output, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return dir, strings.TrimSpace(string(output))
}
