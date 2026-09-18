package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool/builtins"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos-code/internal/worktree"
)

type fakeExecutionRunner struct {
	requests []ExecutionRequest
	result   ExecutionResult
	err      error
	root     string
}

func (r *fakeExecutionRunner) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	r.requests = append(r.requests, request)
	r.root, _ = builtins.WorkspaceRootFromContext(ctx)
	return r.result, r.err
}

type fakePlanWorktrees struct {
	handle     worktree.Handle
	collected  worktree.Result
	created    worktree.CreateOptions
	integrated []string
	canceled   bool
	removed    bool
}

func (m *fakePlanWorktrees) Create(_ context.Context, _ string, options worktree.CreateOptions) (worktree.Handle, error) {
	m.created = options
	return m.handle, nil
}

func (m *fakePlanWorktrees) Collect(_ context.Context, _ worktree.Handle, checks []worktree.Check) (worktree.Result, error) {
	result := m.collected
	result.Checks = append([]worktree.Check(nil), checks...)
	return result, nil
}

func (m *fakePlanWorktrees) Integrate(_ context.Context, _ worktree.Handle, paths []string) (worktree.Result, error) {
	m.integrated = append([]string(nil), paths...)
	return worktree.Result{ChangedPaths: append([]string(nil), paths...), Cleanup: worktree.Cleanup{State: "complete"}}, nil
}

func (m *fakePlanWorktrees) Cancel(context.Context, worktree.Handle) error {
	m.canceled = true
	return nil
}

func (m *fakePlanWorktrees) Remove(context.Context, worktree.Handle) error {
	m.removed = true
	return nil
}

func TestPlanNodeExecutorUsesBoundedBlockingExecutionAndMapsRuntimeEvidence(t *testing.T) {
	runner := &fakeExecutionRunner{result: ExecutionResult{
		Response: &model.ChatResponse{Content: "implemented"}, StopReason: execution.StopSuccess,
		Verification: verification.Decision{Allowed: true}, ChangedPaths: []string{"internal/plan/controller.go"},
		EvidenceIDs: []execution.EvidenceID{"evidence-1"},
		Usage:       model.Usage{PromptTokens: 13, CompletionTokens: 5, CacheReadTokens: 3, CacheCreationTokens: 2}, CostMicrodollars: 9,
	}}
	worktrees := &fakePlanWorktrees{
		handle:    worktree.Handle{Manifest: worktree.Manifest{ID: "workspace", WorktreePath: "/isolated/workspace"}},
		collected: worktree.Result{ChangedPaths: []string{"internal/plan/controller.go"}, Patch: []byte("patch")},
	}
	executor := &planNodeExecutor{runner: runner, worktrees: worktrees, repositoryRoot: "/parent", implementationAgent: "coder"}
	request := plan.NodeExecutionRequest{
		Plan:    plan.Plan{ID: "plan", TaskID: "task", ContextRefs: []plan.ContextRef{{ID: "source", NodeID: "node"}}},
		Node:    plan.Node{ID: "node", Scope: "internal/plan/controller.go", Risks: []string{"must not leak"}, Verification: "go test ./internal/plan"},
		Attempt: "attempt", Context: plan.RestartContext{Entries: []plan.ContextEntry{{ID: "ctx", Content: "bounded context"}}},
	}

	result, err := executor.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("execution requests = %d, want 1", len(runner.requests))
	}
	if runner.root != "/isolated/workspace" || worktrees.created.DirtyPolicy != worktree.DirtyPreserve {
		t.Fatalf("runtime root = %q, create options = %#v", runner.root, worktrees.created)
	}
	got := runner.requests[0]
	if got.Mode != ExecutionBlocking || got.RequestedAgent != "coder" || got.PPD != nil || !got.BoundedContext {
		t.Fatalf("execution request = %#v", got)
	}
	if strings.Contains(got.Message, "must not leak") || !strings.Contains(got.Message, "Scope:\ninternal/plan/controller.go") || !strings.Contains(got.Message, "Context:\nctx: bounded context") || !strings.Contains(got.Message, "Verification:\ngo test ./internal/plan") {
		t.Fatalf("bounded node prompt = %q", got.Message)
	}
	if result.Status != plan.NodeCompleted || result.Verification != plan.VerificationPassed || result.Summary != "implemented" || result.InputTokens != 13 || result.OutputTokens != 5 || result.CostMicrodollars != 9 {
		t.Fatalf("mapped result = %#v", result)
	}
	if !reflect.DeepEqual(result.ChangedPaths, []string{"internal/plan/controller.go"}) || !reflect.DeepEqual(result.EvidenceIDs, []plan.EvidenceID{"evidence-1"}) {
		t.Fatalf("mapped evidence = %#v", result)
	}
	if result.Workspace == nil || len(result.Workspace.Checks) != 1 || !result.Workspace.Checks[0].Passed || !reflect.DeepEqual(worktrees.integrated, result.ChangedPaths) {
		t.Fatalf("workspace result = %#v, integrated = %v", result.Workspace, worktrees.integrated)
	}
}

func TestPlanNodeExecutorAccessIsConservative(t *testing.T) {
	executor := &planNodeExecutor{}
	exact, _ := executor.Access(context.Background(), plan.Plan{}, plan.Node{Scope: "internal/plan/controller.go"})
	ambiguous, _ := executor.Access(context.Background(), plan.Plan{}, plan.Node{Scope: "internal/plan and internal/orchestrator"})
	if !reflect.DeepEqual(exact.Paths, []string{"internal/plan/controller.go"}) || len(ambiguous.Paths) != 0 || exact.ReadOnly || ambiguous.ReadOnly {
		t.Fatalf("access exact=%#v ambiguous=%#v", exact, ambiguous)
	}
}

func TestPlanNodeExecutorCancelsIsolationAfterVerificationFailure(t *testing.T) {
	runner := &fakeExecutionRunner{result: ExecutionResult{
		StopReason:   execution.StopVerificationFailed,
		Verification: verification.Decision{Allowed: false},
	}}
	worktrees := &fakePlanWorktrees{
		handle:    worktree.Handle{Manifest: worktree.Manifest{ID: "workspace", WorktreePath: "/isolated/workspace"}},
		collected: worktree.Result{ChangedPaths: []string{"changed.go"}},
	}
	executor := &planNodeExecutor{runner: runner, worktrees: worktrees, repositoryRoot: "/parent", implementationAgent: "coder"}
	result, err := executor.Execute(context.Background(), plan.NodeExecutionRequest{
		Plan: plan.Plan{TaskID: "task", ID: "plan"}, Node: plan.Node{ID: "node", Scope: "changed.go", Verification: "go test ./..."}, Attempt: "attempt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !worktrees.canceled || len(worktrees.integrated) != 0 {
		t.Fatalf("canceled = %v, integrated = %v", worktrees.canceled, worktrees.integrated)
	}
	if result.Workspace == nil || result.Workspace.Checks[0].Passed || !reflect.DeepEqual(result.ChangedPaths, []string{"changed.go"}) {
		t.Fatalf("failed workspace result = %#v", result)
	}
}

func TestPlanNodeExecutorRequiresIsolationForMutatingNodes(t *testing.T) {
	executor := &planNodeExecutor{runner: &fakeExecutionRunner{}, implementationAgent: "coder"}
	_, err := executor.Execute(context.Background(), plan.NodeExecutionRequest{Plan: plan.Plan{TaskID: "task"}, Node: plan.Node{ID: "node"}, Attempt: "attempt"})
	var capability *IsolationCapabilityError
	if !errors.As(err, &capability) {
		t.Fatalf("error = %v, want IsolationCapabilityError", err)
	}
}
