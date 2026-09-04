package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TaskRun identifies one reproducible execution against a fixture revision.
type TaskRun struct {
	TaskID      string
	RunID       string
	Fixture     string
	Revision    string
	Model       ModelSettings
	Timeout     time.Duration
	Environment []string
}

// TaskExecution is the isolated workspace made available to an adapter.
type TaskExecution struct {
	Workspace   string
	Environment []string
}

// TaskEvent is an adapter event correlated with a task execution.
type TaskEvent struct {
	ID   string
	Name string
}

// TaskExecutionResult contains the data the adapter produces during a run.
type TaskExecutionResult struct {
	Calls          []Call
	Verification   []VerificationEvidence
	Events         []TaskEvent
	ExitCode       int
	LatestChangeAt time.Time
	Grader         GraderOutcome
	Failure        *Failure
}

// TaskAdapter executes an agent in the supplied isolated workspace.
type TaskAdapter interface {
	Run(context.Context, TaskExecution) (TaskExecutionResult, error)
}

// TaskRunner materializes pinned fixture revisions and records their outcomes.
type TaskRunner struct {
	Adapter TaskAdapter
}

// TaskRunResult retains the artifacts needed to inspect a completed or failed run.
type TaskRunResult struct {
	Outcome  TaskOutcome
	Patch    string
	Events   []TaskEvent
	ExitCode int
}

// Run checks out the requested fixture revision, executes the adapter, and
// captures the final workspace diff before the temporary checkout is removed.
func (r TaskRunner) Run(ctx context.Context, run TaskRun) (TaskRunResult, error) {
	if r.Adapter == nil {
		return TaskRunResult{}, fmt.Errorf("eval: task runner requires an adapter")
	}
	if run.TaskID == "" || run.Fixture == "" || run.Revision == "" {
		return TaskRunResult{}, fmt.Errorf("eval: task ID, fixture, and revision are required")
	}
	if run.Model.Provider == "" || run.Model.Name == "" {
		return TaskRunResult{}, fmt.Errorf("eval: model provider and name are required")
	}

	workspace, err := os.MkdirTemp("", "chronos-task-*")
	if err != nil {
		return TaskRunResult{}, fmt.Errorf("eval: create task workspace: %w", err)
	}
	defer os.RemoveAll(workspace)
	if err := runGit(ctx, "clone", "--quiet", run.Fixture, workspace); err != nil {
		return TaskRunResult{}, fmt.Errorf("eval: clone fixture: %w", err)
	}
	if err := runGit(ctx, "-C", workspace, "checkout", "--quiet", "--detach", run.Revision); err != nil {
		return TaskRunResult{}, fmt.Errorf("eval: checkout fixture revision: %w", err)
	}

	runCtx := ctx
	cancel := func() {}
	if run.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, run.Timeout)
	}
	defer cancel()
	execution, adapterErr := r.Adapter.Run(runCtx, TaskExecution{
		Workspace:   workspace,
		Environment: append([]string(nil), run.Environment...),
	})
	// Capture the partial patch even when the task context was cancelled.
	patch, diffErr := gitDiff(context.Background(), workspace)
	if diffErr != nil {
		return TaskRunResult{}, fmt.Errorf("eval: capture task patch: %w", diffErr)
	}

	runID := run.RunID
	if runID == "" {
		runID = run.TaskID + "@" + run.Revision
	}
	outcome := TaskOutcome{
		Version:            TaskOutcomeVersion,
		TaskID:             run.TaskID,
		RunID:              runID,
		RepositoryRevision: run.Revision,
		Model:              run.Model,
		Calls:              execution.Calls,
		Verification:       execution.Verification,
		LatestChangeAt:     execution.LatestChangeAt,
		Grader:             execution.Grader,
		Failure:            execution.Failure,
	}
	if adapterErr != nil {
		outcome.Grader.Passed = false
		outcome.Failure = &Failure{Class: FailureExecution, Message: adapterErr.Error()}
	} else if !outcome.Grader.Passed && outcome.Failure == nil {
		outcome.Failure = &Failure{Class: FailureGrader, Message: "task did not pass grading"}
	}
	return TaskRunResult{Outcome: outcome, Patch: patch, Events: execution.Events, ExitCode: execution.ExitCode}, nil
}

func runGit(ctx context.Context, args ...string) error {
	output, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func gitDiff(ctx context.Context, workspace string) (string, error) {
	if err := runGit(ctx, "-C", workspace, "add", "--intent-to-add", "."); err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, "git", "-C", filepath.Clean(workspace), "diff", "--binary", "--no-ext-diff", "HEAD")
	output, err := command.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return "", err
		}
	}
	return string(output), nil
}
