package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool/builtins"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos-code/internal/worktree"
)

type isolatedWriteRunner struct{ root string }

func (r *isolatedWriteRunner) Execute(ctx context.Context, _ ExecutionRequest) (ExecutionResult, error) {
	r.root, _ = builtins.WorkspaceRootFromContext(ctx)
	if err := os.WriteFile(filepath.Join(r.root, "tracked.txt"), []byte("isolated\n"), 0o644); err != nil {
		return ExecutionResult{}, err
	}
	return ExecutionResult{
		Response: &model.ChatResponse{Content: "implemented"}, StopReason: execution.StopSuccess,
		Verification: verification.Decision{Allowed: true},
	}, nil
}

func TestPlanNodeExecutorIsolatesThenIntegratesVerifiedChanges(t *testing.T) {
	repo := initializePlanExecutorRepo(t)
	parentOnly := filepath.Join(repo, "parent-only.txt")
	if err := os.WriteFile(parentOnly, []byte("preserve\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.New(filepath.Join(t.TempDir(), "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := &isolatedWriteRunner{}
	executor := &planNodeExecutor{runner: runner, worktrees: manager, repositoryRoot: repo, implementationAgent: "coder"}
	result, err := executor.Execute(context.Background(), plan.NodeExecutionRequest{
		Plan: plan.Plan{TaskID: "task", ID: "plan"},
		Node: plan.Node{ID: "node", Scope: "tracked.txt", Verification: "verified by fake runner"}, Attempt: "attempt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner.root == "" || runner.root == repo {
		t.Fatalf("execution root = %q, want isolated worktree", runner.root)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "tracked.txt")); err != nil || string(data) != "isolated\n" {
		t.Fatalf("integrated tracked file = %q, %v", data, err)
	}
	if data, err := os.ReadFile(parentOnly); err != nil || string(data) != "preserve\n" {
		t.Fatalf("parent dirty file = %q, %v", data, err)
	}
	if result.Workspace == nil || result.Workspace.Cleanup.State != "complete" || len(result.ChangedPaths) != 1 || result.ChangedPaths[0] != "tracked.txt" {
		t.Fatalf("result = %#v", result)
	}
	if handles, err := manager.Recover(); err != nil || len(handles) != 0 {
		t.Fatalf("recovered handles = %#v, %v", handles, err)
	}
}

func initializePlanExecutorRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runPlanExecutorGit(t, repo, "init")
	runPlanExecutorGit(t, repo, "config", "user.name", "Chronos Test")
	runPlanExecutorGit(t, repo, "config", "user.email", "chronos@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runPlanExecutorGit(t, repo, "add", "tracked.txt")
	runPlanExecutorGit(t, repo, "commit", "-m", "base")
	return repo
}

func runPlanExecutorGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
