package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos-code/internal/worktree"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
)

type executionRunner interface {
	Execute(context.Context, ExecutionRequest) (ExecutionResult, error)
}

type planWorktreeManager interface {
	Create(context.Context, string, worktree.CreateOptions) (worktree.Handle, error)
	Collect(context.Context, worktree.Handle, []worktree.Check) (worktree.Result, error)
	Integrate(context.Context, worktree.Handle, []string) (worktree.Result, error)
	Cancel(context.Context, worktree.Handle) error
	Remove(context.Context, worktree.Handle) error
}

type verifiedPlanIntegrator interface {
	IntegrateVerified(context.Context, worktree.Handle, []string, []worktree.Check, string) (worktree.Result, error)
}

// IsolationCapabilityError reports that a mutating execution cannot be bound
// to an isolated workspace. Callers may use errors.As to handle it explicitly.
type IsolationCapabilityError struct{ Reason string }

func (e *IsolationCapabilityError) Error() string {
	return "isolated workspace capability unavailable: " + e.Reason
}

// planNodeExecutor adapts a durable node to the ordinary execution path. The
// explicit agent selection prevents Execute from routing the node back to the strategist.
type planNodeExecutor struct {
	runner              executionRunner
	worktrees           planWorktreeManager
	repositoryRoot      string
	implementationAgent string
	permissionChecker   *security.PermissionChecker
}

func (e *planNodeExecutor) Execute(ctx context.Context, request plan.NodeExecutionRequest) (plan.NodeExecutionResult, error) {
	if e.runner == nil || e.implementationAgent == "" {
		return plan.NodeExecutionResult{}, fmt.Errorf("execute plan node: missing implementation executor")
	}
	access, _ := e.Access(ctx, request.Plan, request.Node)
	if access.ReadOnly {
		return e.execute(ctx, request, access.Paths)
	}
	if e.worktrees == nil || e.repositoryRoot == "" {
		return plan.NodeExecutionResult{}, &plan.StopError{
			Reason: plan.StopCapabilityMissing,
			Err:    &IsolationCapabilityError{Reason: "mutating plan nodes require a worktree manager and repository root"},
		}
	}
	if len(access.Paths) == 0 {
		return plan.NodeExecutionResult{}, &plan.StopError{Reason: plan.StopCapabilityMissing, Err: fmt.Errorf("mutating plan node %q has no exact authorized scope", request.Node.ID)}
	}
	predecessors, err := request.Plan.AcceptedArtifactIDs(request.Node.ID)
	if err != nil {
		return plan.NodeExecutionResult{}, &plan.StopError{Reason: plan.StopAmbiguity, Err: fmt.Errorf("resolve plan node inputs: %w", err)}
	}
	var allowedDirty []string
	for _, path := range access.Paths {
		if e.permissionChecker != nil && e.permissionChecker.CheckContext(builtins.WithWorkspaceRoot(ctx, e.repositoryRoot), "file_read", map[string]any{"path": path}, false) == security.Auto {
			allowedDirty = append(allowedDirty, path)
		}
	}
	handle, err := e.worktrees.Create(ctx, e.repositoryRoot, worktree.CreateOptions{
		TaskID: string(request.Plan.TaskID), AttemptID: string(request.Attempt), DirtyPolicy: worktree.DirtyPreserve,
		AcceptedArtifacts: predecessors, AuthorizedDirtyPaths: allowedDirty,
	})
	if err != nil {
		if handle.Manifest.ID != "" {
			_ = e.cancel(handle)
		}
		return plan.NodeExecutionResult{}, fmt.Errorf("create plan node worktree: %w", err)
	}

	runCtx := builtins.WithWorkspaceRoot(ctx, handle.Manifest.WorktreePath)
	runCtx = tool.WithScratchWorkspace(runCtx)
	mapped, runErr := e.execute(runCtx, request, access.Paths)
	passed := runErr == nil && mapped.Status == plan.NodeCompleted && mapped.StopReason == "" && mapped.Verification == plan.VerificationPassed
	check := worktree.Check{Name: "plan-node-verification", Passed: passed}
	if !passed {
		check.Detail = string(mapped.StopReason)
		if runErr != nil {
			check.Detail = runErr.Error()
		}
	}
	collectCtx, collectCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	collected, collectErr := e.worktrees.Collect(collectCtx, handle, []worktree.Check{check})
	collectCancel()
	if collectErr != nil {
		cancelErr := e.cancel(handle)
		return plan.NodeExecutionResult{}, errors.Join(fmt.Errorf("collect plan node worktree: %w", collectErr), cancelErr)
	}
	mapped.Workspace = &collected
	mapped.ChangedPaths = append([]string(nil), collected.ChangedPaths...)
	for _, path := range collected.ChangedPaths {
		allowed := false
		for _, scope := range access.Paths {
			if path == scope || strings.HasPrefix(path, scope+"/") {
				allowed = true
				break
			}
		}
		if !allowed {
			return mapped, errors.Join(&plan.StopError{Reason: plan.StopApprovalDenied, Err: fmt.Errorf("plan node wrote outside declared scope: %s", path)}, e.cancel(handle))
		}
	}
	if !passed {
		cancelErr := e.cancel(handle)
		return mapped, errors.Join(runErr, cancelErr)
	}
	if len(collected.ChangedPaths) == 0 {
		if err := e.remove(handle); err != nil {
			return mapped, err
		}
		mapped.Workspace.Cleanup.State = "complete"
		return mapped, nil
	}
	var integrated worktree.Result
	if lease, ok := execution.OperationLeaseFromContext(ctx); ok {
		err = lease.Store.WithLeaseEffect(ctx, lease.Lease, 2*time.Minute, func(effectCtx context.Context) error {
			var applyErr error
			integrated, applyErr = e.integrateVerified(effectCtx, handle, collected.ChangedPaths, collected.Checks, collected.FinalHash)
			return applyErr
		})
	} else {
		integrated, err = e.integrateVerified(ctx, handle, collected.ChangedPaths, collected.Checks, collected.FinalHash)
	}
	if err != nil {
		if integrated.ArtifactID != "" && (integrated.Cleanup.State == string(worktree.CleanupPending) || integrated.Cleanup.State == "complete") {
			mapped.Workspace = &integrated
			return mapped, &plan.StopError{Reason: plan.StopAmbiguity, Err: fmt.Errorf("plan node integration needs receipt reconciliation: %w", err)}
		}
		cancelErr := e.cancel(handle)
		return mapped, errors.Join(fmt.Errorf("integrate plan node worktree: %w", err), cancelErr)
	}
	mapped.ChangedPaths = append([]string(nil), integrated.ChangedPaths...)
	mapped.Workspace.ArtifactID = integrated.ArtifactID
	mapped.Workspace.ReceiptID = integrated.ReceiptID
	mapped.Workspace.Cleanup = integrated.Cleanup
	return mapped, nil
}

func (e *planNodeExecutor) integrateVerified(ctx context.Context, handle worktree.Handle, paths []string, checks []worktree.Check, expectedHash string) (worktree.Result, error) {
	if integrator, ok := e.worktrees.(verifiedPlanIntegrator); ok {
		return integrator.IntegrateVerified(ctx, handle, paths, checks, expectedHash)
	}
	return e.worktrees.Integrate(ctx, handle, paths)
}

func (e *planNodeExecutor) execute(ctx context.Context, request plan.NodeExecutionRequest, paths []string) (plan.NodeExecutionResult, error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}
	if identity, ok := agent.RunIdentityFromContext(ctx); ok {
		identity.NodeID = string(request.Node.ID)
		identity.AttemptID = string(request.Attempt)
		ctx = agent.WithRunIdentity(ctx, identity)
	}
	result, err := e.runner.Execute(ctx, ExecutionRequest{
		Message:          nodeImplementationPrompt(request),
		Mode:             ExecutionBlocking,
		RequestedAgent:   e.implementationAgent,
		SessionID:        fmt.Sprintf("plan-%s-%s", request.Plan.ID, request.Attempt),
		TaskID:           fmt.Sprintf("%s-%s-%s", request.Plan.TaskID, request.Node.ID, request.Attempt),
		BoundedContext:   true,
		VerificationMode: verification.ModeEnforce,
		VerificationObligations: []verification.Obligation{{
			ID: "plan-node-verification", Command: request.Node.Verification, Paths: paths,
		}},
	})
	mapped := mapNodeExecutionResult(request, result, err)
	if err != nil && mapped.StopReason == "" {
		return plan.NodeExecutionResult{}, err
	}
	return mapped, nil
}

func (e *planNodeExecutor) cancel(handle worktree.Handle) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.worktrees.Cancel(ctx, handle); err != nil {
		return fmt.Errorf("cancel plan node worktree (manifest retained): %w", err)
	}
	return nil
}

func (e *planNodeExecutor) remove(handle worktree.Handle) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.worktrees.Remove(ctx, handle); err != nil {
		return fmt.Errorf("remove empty plan node worktree (manifest retained): %w", err)
	}
	return nil
}

func (e *planNodeExecutor) Access(_ context.Context, _ plan.Plan, node plan.Node) (plan.NodeAccess, error) {
	scope := strings.TrimSpace(node.Scope)
	if scope == "" || strings.ContainsAny(scope, "\t\r\n ,;*?[]{}") || filepath.IsAbs(scope) || filepath.Ext(scope) == "" {
		return plan.NodeAccess{}, nil
	}
	path := filepath.ToSlash(filepath.Clean(scope))
	if path == "." || path == ".." || strings.HasPrefix(path, "../") {
		return plan.NodeAccess{}, nil
	}
	return plan.NodeAccess{Paths: []string{path}}, nil
}

func nodeImplementationPrompt(request plan.NodeExecutionRequest) string {
	var contextLines []string
	for _, entry := range request.Context.Entries {
		contextLines = append(contextLines, fmt.Sprintf("%s: %s", entry.ID, entry.Content))
	}
	if len(contextLines) == 0 {
		for _, ref := range request.Plan.ContextRefs {
			if ref.NodeID == request.Node.ID {
				contextLines = append(contextLines, string(ref.ID))
			}
		}
	}
	sort.Strings(contextLines)
	evidenceNodes := map[plan.NodeID]bool{request.Node.ID: true}
	for _, dependency := range request.Plan.Dependencies {
		if dependency.NodeID == request.Node.ID {
			evidenceNodes[dependency.DependsOn] = true
		}
	}
	var evidence []string
	for _, item := range request.Plan.Evidence {
		if evidenceNodes[item.NodeID] {
			evidence = append(evidence, string(item.ID))
		}
	}
	sort.Strings(evidence)
	return fmt.Sprintf("Kind:\n%s\n\nObjective:\n%s\n\nScope boundary (not the task instruction):\n%s\n\nExpected artifacts:\n%s\n\nRisks:\n%s\n\nAssumptions:\n%s\n\nInvalidation triggers:\n%s\n\nRecovery class:\n%s\n\nContext references/evidence:\n%s\n\nVerification:\n%s",
		request.Node.Kind, request.Node.Objective, request.Node.Scope,
		strings.Join(request.Node.ExpectedArtifacts, "\n"), strings.Join(request.Node.Risks, "\n"),
		strings.Join(request.Node.Assumptions, "\n"), strings.Join(request.Node.InvalidationTriggers, "\n"), request.Node.RecoveryClass,
		strings.Join(append(contextLines, evidence...), "\n"), request.Node.Verification)
}

func mapNodeExecutionResult(request plan.NodeExecutionRequest, result ExecutionResult, err error) plan.NodeExecutionResult {
	mapped := plan.NodeExecutionResult{
		NodeID: request.Node.ID, AttemptID: request.Attempt,
		Status: plan.NodeCompleted, Verification: plan.VerificationPassed,
		ChangedPaths: append([]string(nil), result.ChangedPaths...),
		InputTokens:  int64(result.Usage.UncachedPromptTokens()), OutputTokens: int64(result.Usage.CompletionTokens),
		CacheReadTokens: int64(result.Usage.CacheReadTokens), CacheCreationTokens: int64(result.Usage.CacheCreationTokens),
		CostMicrodollars: result.CostMicrodollars,
	}
	for _, id := range result.EvidenceIDs {
		mapped.EvidenceIDs = append(mapped.EvidenceIDs, plan.EvidenceID(id))
	}
	if result.Response != nil {
		mapped.Summary = result.Response.Content
	}
	if err == nil && result.StopReason == execution.StopSuccess && result.Verification.Allowed && !result.Verification.Disagreement {
		return mapped
	}
	mapped.Verification = plan.VerificationFailed
	reason := result.StopReason
	if reason == "" {
		reason = execution.StopReasonForError(err)
	}
	switch reason {
	case execution.StopBudgetExhausted:
		mapped.Status, mapped.StopReason = plan.NodeBlocked, plan.StopBudgetExhausted
	case execution.StopVerificationFailed:
		mapped.Status, mapped.StopReason = plan.NodeFailed, plan.StopVerificationFailed
	case execution.StopPolicyDenied:
		mapped.Status, mapped.StopReason = plan.NodeBlocked, plan.StopApprovalDenied
	case execution.StopAuthentication:
		mapped.Status, mapped.StopReason = plan.NodeBlocked, plan.StopCapabilityMissing
	case execution.StopCancelled:
		mapped.Status, mapped.StopReason = plan.NodeCanceled, plan.StopUserDecisionRequired
	case execution.StopRepeatedFailure:
		mapped.Status, mapped.StopReason = plan.NodeFailed, plan.StopRetryExhausted
	default:
		var terminal *execution.TerminalError
		if err != nil && (errors.As(err, &terminal) && terminal.Retryable || reason == execution.StopProviderRetryable || reason == execution.StopTimeout || reason == execution.StopInternalError) {
			return plan.NodeExecutionResult{}
		}
		mapped.Status, mapped.StopReason = plan.NodeBlocked, plan.StopReason(reason)
		if reason == execution.StopSuccess {
			mapped.Status, mapped.StopReason = plan.NodeFailed, plan.StopVerificationFailed
		}
	}
	return mapped
}
