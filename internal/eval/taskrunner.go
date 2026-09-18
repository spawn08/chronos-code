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
	FixtureType string
	Revision    string
	Model       ModelSettings
	Timeout     time.Duration
	Environment []string
	Prompt      string
	Permissions []string
	Grading     HiddenGraderSpec
	Ablation    Ablation
}

// TaskExecution is the isolated workspace made available to an adapter.
type TaskExecution struct {
	Workspace    string
	Environment  []string
	Instructions string
	Permissions  []string
	Ablation     Ablation
}

// TaskEvent is an adapter event correlated with a task execution.
type TaskEvent struct {
	ID   string
	Name string
}

// TaskExecutionResult contains the data the adapter produces during a run.
type TaskExecutionResult struct {
	Calls            []Call
	Verification     []VerificationEvidence
	Events           []TaskEvent
	Patch            string
	ExitCode         int
	TerminalStatus   string
	CostMicrodollars int64
	RetryAttempts    int
	RepairAttempts   int
	LatestChangeAt   time.Time
	Grader           GraderOutcome
	Failure          *Failure
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
	if run.FixtureType == "local" {
		if err := copyFixture(run.Fixture, workspace); err != nil {
			return TaskRunResult{}, fmt.Errorf("eval: copy fixture: %w", err)
		}
		if err := initializeFixtureRepository(ctx, workspace); err != nil {
			return TaskRunResult{}, fmt.Errorf("eval: initialize copied fixture: %w", err)
		}
	} else {
		if err := runGit(ctx, "clone", "--quiet", run.Fixture, workspace); err != nil {
			return TaskRunResult{}, fmt.Errorf("eval: clone fixture: %w", err)
		}
		if err := runGit(ctx, "-C", workspace, "checkout", "--quiet", "--detach", run.Revision); err != nil {
			return TaskRunResult{}, fmt.Errorf("eval: checkout fixture revision: %w", err)
		}
	}

	runCtx := ctx
	cancel := func() {}
	if run.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, run.Timeout)
	}
	defer cancel()
	execution, adapterErr := r.Adapter.Run(runCtx, TaskExecution{
		Workspace:    workspace,
		Environment:  append([]string(nil), run.Environment...),
		Instructions: run.Prompt,
		Permissions:  append([]string(nil), run.Permissions...),
		Ablation:     run.Ablation,
	})
	// Capture the partial patch even when the task context was cancelled.
	patch, diffErr := workspaceDiff(context.Background(), workspace, run.FixtureType)
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
		TerminalStatus:     execution.TerminalStatus,
		Patch:              patch,
		CostMicrodollars:   execution.CostMicrodollars,
		RetryAttempts:      execution.RetryAttempts,
		RepairAttempts:     execution.RepairAttempts,
		Verification:       execution.Verification,
		LatestChangeAt:     execution.LatestChangeAt,
		Grader:             execution.Grader,
		Failure:            execution.Failure,
	}
	if adapterErr == nil && (len(run.Grading.Commands) > 0 || len(run.Grading.Artifacts) > 0) {
		graderCtx := context.Background()
		graderCancel := func() {}
		if run.Timeout > 0 {
			graderCtx, graderCancel = context.WithTimeout(graderCtx, run.Timeout)
		}
		grade := runHiddenGrader(graderCtx, workspace, run.Environment, run.Grading)
		graderCancel()
		outcome.Grader = GraderOutcome{Passed: grade.Passed && execution.Grader.Passed, Name: "deterministic-v1"}
		outcome.Verification = append(outcome.Verification, grade.Evidence...)
		if !grade.Passed {
			outcome.Failure = &Failure{Class: FailureGrader, Message: strings.Join(grade.Failures, "; ")}
		} else if outcome.Grader.Passed {
			outcome.Failure = nil
		}
	}
	if adapterErr != nil {
		outcome.Grader.Passed = false
		outcome.Failure = &Failure{Class: FailureExecution, Message: adapterErr.Error()}
	} else if !outcome.Grader.Passed && outcome.Failure == nil {
		outcome.Failure = &Failure{Class: FailureGrader, Message: "task did not pass grading"}
	}
	return TaskRunResult{Outcome: outcome, Patch: patch, Events: execution.Events, ExitCode: execution.ExitCode}, nil
}

func copyFixture(source, destination string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink %s is not allowed", relative)
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func workspaceDiff(ctx context.Context, workspace, _ string) (string, error) {
	return gitDiff(ctx, workspace)
}

func initializeFixtureRepository(ctx context.Context, workspace string) error {
	for _, args := range [][]string{
		{"-C", workspace, "init", "--quiet"},
		{"-C", workspace, "add", "."},
		{"-C", workspace, "-c", "user.name=chronos-eval", "-c", "user.email=eval@invalid", "commit", "--quiet", "-m", "fixture"},
	} {
		if err := runGit(ctx, args...); err != nil {
			return err
		}
	}
	return nil
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
