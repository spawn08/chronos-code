package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		Plan: plan.Plan{TaskID: "task", ID: "plan", Nodes: []plan.Node{{ID: "node", State: plan.NodeRunning}}},
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

type acceptedChainRunner struct{ observedAPI bool }

func (r *acceptedChainRunner) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	root, _ := builtins.WorkspaceRootFromContext(ctx)
	if strings.Contains(request.TaskID, "-a-") {
		if err := os.WriteFile(filepath.Join(root, "api.go"), []byte("package fixture\nfunc API() int { return 42 }\n"), 0o644); err != nil {
			return ExecutionResult{}, err
		}
	} else {
		api, err := os.ReadFile(filepath.Join(root, "api.go"))
		if err != nil || !strings.Contains(string(api), "func API()") {
			return ExecutionResult{}, fmt.Errorf("dependent node cannot see accepted API: %w", err)
		}
		r.observedAPI = true
		if err := os.WriteFile(filepath.Join(root, "use.go"), []byte("package fixture\nvar Value = API()\n"), 0o644); err != nil {
			return ExecutionResult{}, err
		}
	}
	return ExecutionResult{StopReason: execution.StopSuccess, Verification: verification.Decision{Allowed: true}}, nil
}

func TestPlanControllerResumesDependentAgainstAcceptedArtifactNotHEAD(t *testing.T) {
	ctx := context.Background()
	repo := initializePlanExecutorRepo(t)
	manager, err := worktree.New(filepath.Join(t.TempDir(), "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := plan.OpenSQLStore(ctx, filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := plan.Plan{TenantID: "tenant", RepositoryID: "repo", TaskID: "task", ID: "plan", Generation: "one", State: plan.PlanActive,
		Nodes:        []plan.Node{{ID: "a", State: plan.NodePending, Kind: plan.NodeImplement, Scope: "api.go", Verification: "verified by runner"}, {ID: "b", State: plan.NodePending, Kind: plan.NodeImplement, Scope: "use.go", Verification: "verified by runner"}},
		Dependencies: []plan.Dependency{{NodeID: "b", DependsOn: "a"}},
	}
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	runner := &acceptedChainRunner{}
	controller := plan.NewController(store, &planNodeExecutor{runner: runner, worktrees: manager, repositoryRoot: repo, implementationAgent: "coder"}, nil, nil, plan.ControllerConfig{})
	completed, err := controller.Run(ctx, p)
	if err != nil || completed.State != plan.PlanCompleted || !runner.observedAPI || len(completed.Artifacts) != 2 {
		t.Fatalf("accepted dependent result = %+v, observed API = %v, error = %v", completed, runner.observedAPI, err)
	}
	if output, err := os.ReadFile(filepath.Join(repo, "use.go")); err != nil || !strings.Contains(string(output), "API()") {
		t.Fatalf("parent integration = %q, error = %v", output, err)
	}
	if again, err := controller.Run(ctx, p); err != nil || again.State != plan.PlanCompleted || len(again.Attempts) != 2 {
		t.Fatalf("resumed completed plan = %+v, error = %v", again, err)
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
