package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"

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
	identity agent.RunIdentity
}

func (r *fakeExecutionRunner) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	r.requests = append(r.requests, request)
	r.root, _ = builtins.WorkspaceRootFromContext(ctx)
	r.identity, _ = agent.RunIdentityFromContext(ctx)
	return r.result, r.err
}

type fakePlanWorktrees struct {
	handle            worktree.Handle
	collected         worktree.Result
	created           worktree.CreateOptions
	integrated        []string
	canceled          bool
	removed           bool
	integrationError  error
	integrationResult worktree.Result
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
	if m.integrationError != nil {
		return m.integrationResult, m.integrationError
	}
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
		Plan:    plan.Plan{ID: "plan", TaskID: "task", Nodes: []plan.Node{{ID: "prior", State: plan.NodeCompleted}, {ID: "node", State: plan.NodeRunning}}, ContextRefs: []plan.ContextRef{{ID: "source", NodeID: "node"}}, Dependencies: []plan.Dependency{{NodeID: "node", DependsOn: "prior"}}, Evidence: []plan.Evidence{{ID: "evidence-prior", NodeID: "prior"}}},
		Node:    plan.Node{ID: "node", Kind: plan.NodeImplement, Objective: "implement durable node metadata", Scope: "internal/plan/controller.go", ExpectedArtifacts: []string{"controller patch"}, Assumptions: []string{"store migration is present"}, InvalidationTriggers: []string{"node schema changes"}, RecoveryClass: plan.RecoveryReplan, Risks: []string{"metadata may be lost"}, Verification: "go test ./internal/plan"},
		Attempt: "attempt", Context: plan.RestartContext{Entries: []plan.ContextEntry{{ID: "ctx", Content: "bounded context"}}},
	}

	parent := agent.RunIdentity{DeliveryID: "delivery", NodeID: "previous", AttemptID: "previous", RoleID: "worker", InvocationID: "worker-run"}
	result, err := executor.Execute(agent.WithRunIdentity(context.Background(), parent), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("execution requests = %d, want 1", len(runner.requests))
	}
	if runner.root != "/isolated/workspace" || worktrees.created.DirtyPolicy != worktree.DirtyPreserve {
		t.Fatalf("runtime root = %q, create options = %#v", runner.root, worktrees.created)
	}
	if runner.identity.DeliveryID != parent.DeliveryID || runner.identity.NodeID != "node" || runner.identity.AttemptID != "attempt" || runner.identity.InvocationID != parent.InvocationID {
		t.Fatalf("plan node identity = %+v", runner.identity)
	}
	got := runner.requests[0]
	if got.Mode != ExecutionBlocking || got.RequestedAgent != "coder" || got.PPD != nil || !got.BoundedContext {
		t.Fatalf("execution request = %#v", got)
	}
	for _, required := range []string{"Objective:\nimplement durable node metadata", "Scope boundary (not the task instruction):\ninternal/plan/controller.go", "Expected artifacts:\ncontroller patch", "Risks:\nmetadata may be lost", "Context references/evidence:\nctx: bounded context\nevidence-prior", "Verification:\ngo test ./internal/plan"} {
		if !strings.Contains(got.Message, required) {
			t.Fatalf("bounded node prompt missing %q: %q", required, got.Message)
		}
	}
	if strings.Contains(got.Message, "Scope:\n") {
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
		Plan: plan.Plan{TaskID: "task", ID: "plan", Nodes: []plan.Node{{ID: "node", State: plan.NodeRunning}}}, Node: plan.Node{ID: "node", Scope: "changed.go", Verification: "go test ./..."}, Attempt: "attempt",
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

func TestPlanNodeExecutorParksAppliedPatchWhenCleanupFails(t *testing.T) {
	runner := &fakeExecutionRunner{result: ExecutionResult{StopReason: execution.StopSuccess, Verification: verification.Decision{Allowed: true}}}
	worktrees := &fakePlanWorktrees{
		handle:            worktree.Handle{Manifest: worktree.Manifest{ID: "workspace", WorktreePath: "/isolated/workspace"}},
		collected:         worktree.Result{ChangedPaths: []string{"changed.go"}},
		integrationResult: worktree.Result{ArtifactID: "sha256:accepted", ChangedPaths: []string{"changed.go"}, Cleanup: worktree.Cleanup{State: string(worktree.CleanupPending)}},
		integrationError:  errors.New("cleanup failed after patch applied"),
	}
	executor := &planNodeExecutor{runner: runner, worktrees: worktrees, repositoryRoot: "/parent", implementationAgent: "coder"}
	result, err := executor.Execute(context.Background(), plan.NodeExecutionRequest{Plan: plan.Plan{TaskID: "task", Nodes: []plan.Node{{ID: "node", State: plan.NodeRunning}}}, Node: plan.Node{ID: "node", Scope: "changed.go", Verification: "go test ./..."}, Attempt: "attempt"})
	var stopped *plan.StopError
	if !errors.As(err, &stopped) || stopped.Reason != plan.StopAmbiguity || worktrees.canceled || result.Workspace == nil || result.Workspace.ArtifactID != "sha256:accepted" {
		t.Fatalf("applied patch outcome = %+v, error = %v, canceled = %v", result, err, worktrees.canceled)
	}
}

func TestPlanNodeExecutorComposesAcceptedPredecessorsFromPersistedPlan(t *testing.T) {
	runner := &fakeExecutionRunner{result: ExecutionResult{StopReason: execution.StopSuccess, Verification: verification.Decision{Allowed: true}}}
	worktrees := &fakePlanWorktrees{
		handle:    worktree.Handle{Manifest: worktree.Manifest{ID: "workspace", WorktreePath: "/isolated/workspace"}},
		collected: worktree.Result{},
	}
	executor := &planNodeExecutor{runner: runner, worktrees: worktrees, repositoryRoot: "/parent", implementationAgent: "coder"}
	request := plan.NodeExecutionRequest{
		Plan: plan.Plan{TaskID: "task", Nodes: []plan.Node{{ID: "a", State: plan.NodeCompleted}, {ID: "b", State: plan.NodeCompleted}, {ID: "c", State: plan.NodeRunning}, {ID: "unrelated", State: plan.NodeCompleted}},
			Dependencies: []plan.Dependency{{NodeID: "c", DependsOn: "b"}, {NodeID: "b", DependsOn: "a"}},
			Artifacts:    []plan.Artifact{{NodeID: "a", ID: "sha256:first"}, {NodeID: "b", ID: "sha256:second"}, {NodeID: "unrelated", ID: "sha256:excluded"}}},
		Node: plan.Node{ID: "c", Scope: "changed.go", Verification: "go test ./..."}, Attempt: "attempt",
	}
	if _, err := executor.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(worktrees.created.AcceptedArtifacts, []string{"sha256:first", "sha256:second"}) {
		t.Fatalf("private predecessor snapshot = %v", worktrees.created.AcceptedArtifacts)
	}
}

func TestPlanNodeExecutorRejectsOutOfScopeMutationBeforeIntegration(t *testing.T) {
	runner := &fakeExecutionRunner{result: ExecutionResult{StopReason: execution.StopSuccess, Verification: verification.Decision{Allowed: true}}}
	worktrees := &fakePlanWorktrees{
		handle:    worktree.Handle{Manifest: worktree.Manifest{ID: "workspace", WorktreePath: "/isolated/workspace"}},
		collected: worktree.Result{ChangedPaths: []string{"private.txt"}},
	}
	executor := &planNodeExecutor{runner: runner, worktrees: worktrees, repositoryRoot: "/parent", implementationAgent: "coder"}
	_, err := executor.Execute(context.Background(), plan.NodeExecutionRequest{
		Plan: plan.Plan{TaskID: "task", Nodes: []plan.Node{{ID: "node", State: plan.NodeRunning}}},
		Node: plan.Node{ID: "node", Scope: "allowed.go", Verification: "go test ./..."}, Attempt: "attempt",
	})
	var stopped *plan.StopError
	if !errors.As(err, &stopped) || stopped.Reason != plan.StopApprovalDenied || !worktrees.canceled || len(worktrees.integrated) != 0 {
		t.Fatalf("out-of-scope patch = %v, canceled=%t integrated=%v", err, worktrees.canceled, worktrees.integrated)
	}
}
