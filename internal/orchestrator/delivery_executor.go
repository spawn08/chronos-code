package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos/sdk/agent"
)

// ReadOnlyDeliveryExecutor is a bounded bridge from worker-owned leases to the
// existing common execution path. It parks outcomes for acceptance verification
// rather than claiming autonomous completion before F06/F07/F10 are wired.
type ReadOnlyDeliveryExecutor struct {
	orchestrator *Orchestrator
	authorizer   authorization.Authorizer
	sandbox      security.SandboxPolicy
}

func NewReadOnlyDeliveryExecutor(orch *Orchestrator, authorizer authorization.Authorizer, sandbox security.SandboxPolicy) (*ReadOnlyDeliveryExecutor, error) {
	if orch == nil || authorizer == nil {
		return nil, execution.ErrInvalidDelivery
	}
	return &ReadOnlyDeliveryExecutor{orchestrator: orch, authorizer: authorizer, sandbox: sandbox}, nil
}

// SupportsCappedDelivery is conservative: every reachable configured role
// must have the durable model-call hook. Uninstrumented dynamic roles are
// refused by the delegated runner under a worker lease.
func (e *ReadOnlyDeliveryExecutor) SupportsCappedDelivery() bool {
	if e == nil || e.orchestrator == nil || len(e.orchestrator.agents) == 0 {
		return false
	}
	for _, a := range e.orchestrator.agents {
		if a == nil {
			return false
		}
		found := false
		for _, hook := range a.Hooks {
			if _, ok := hook.(deliveryBudgetHook); ok {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (e *ReadOnlyDeliveryExecutor) Execute(ctx context.Context, executionAttempt *execution.Execution) execution.Outcome {
	delivery := executionAttempt.Lease.Delivery
	request := authorization.Request{
		PrincipalID: "delivery-worker", TenantID: string(delivery.TenantID),
		RepositoryID: string(delivery.RepositoryID), Action: "delivery.execute",
	}
	if err := e.authorizer.Authorize(ctx, request); err != nil {
		return deliveryWait("authorization-required", fmt.Errorf("authorize delivery worker: %w", err))
	}
	if delivery.ID == "" || len(delivery.Goals) == 0 || delivery.State != execution.DeliveryRunning {
		return deliveryWait("invalid-delivery", execution.ErrInvalidDelivery)
	}
	operations, err := executionAttempt.PriorOperations(ctx)
	if err != nil {
		return deliveryWait("effect-reconciliation", fmt.Errorf("load prior delivery effects: %w", err))
	}
	for _, op := range operations {
		if op.Attempt != executionAttempt.Lease.Attempt {
			if op.Status == execution.OperationRunning && op.ReplayClass == execution.ReplayFingerprintedWrite && e.orchestrator.workspace != nil {
				root, err := filepath.EvalSymlinks(e.orchestrator.workspace.Root)
				if err == nil {
					if path, safe := safeObservationPath(root, op.ObservationPath); safe && op.ExpectedOutputFingerprint != "" {
						state := readFileState(root, path)
						if state.exists && state.hash == op.ExpectedOutputFingerprint {
							receipt, err := json.Marshal(map[string]any{"path": filepath.Join(root, path), "bytes_written": state.size})
							if err != nil {
								return deliveryWait("effect-reconciliation", fmt.Errorf("encode observed write receipt: %w", err))
							}
							if _, err := executionAttempt.ReconcileOperation(ctx, op.ID, "file-content:"+state.hash, state.hash, receipt); err != nil {
								return deliveryWait("effect-reconciliation", fmt.Errorf("reconcile observed file write: %w", err))
							}
						}
					}
				}
			}
			return deliveryWait("effect-reconciliation", execution.ErrEffectNeedsReconciliation)
		}
	}
	usage, err := executionAttempt.CumulativeUsage(ctx)
	if err != nil {
		return deliveryWait("usage-reconciliation", fmt.Errorf("load delivery usage: %w", err))
	}
	if usage.OutstandingCalls > 0 {
		return deliveryWait("usage-reconciliation", execution.ErrUsageOutcomeUnknown)
	}
	if _, hasIntent, err := memory.ParseIntent(delivery.Goals[len(delivery.Goals)-1].Statement); err != nil || hasIntent {
		return deliveryWait("read-only-mode", fmt.Errorf("memory mutations are unavailable in read-only delivery: %w", execution.ErrInvalidDelivery))
	}
	ctx = authorization.WithRequest(ctx, request)
	ctx = agent.WithRunIdentity(ctx, agent.RunIdentity{
		TenantID: string(delivery.TenantID), RepositoryID: string(delivery.RepositoryID),
		DeliveryID: string(delivery.ID), TaskID: string(delivery.ID), RoleID: "delivery-worker",
		InvocationID: fmt.Sprintf("delivery:%s:%d", delivery.ID, executionAttempt.Lease.Epoch),
	})
	ctx = executionAttempt.OperationContext(ctx)
	ctx = agent.WithModelRetriesDisabled(ctx)
	ctx = security.WithEffectGrant(ctx, security.EffectRead)
	ctx = security.WithMandatorySandbox(ctx, e.sandbox)
	result, err := e.orchestrator.Execute(ctx, ExecutionRequest{
		Message: delivery.Goals[len(delivery.Goals)-1].Statement,
		TaskID:  string(delivery.ID), SessionID: "delivery:" + string(delivery.ID), RequestedAgent: e.orchestrator.active, BoundedContext: true,
	})
	if err != nil {
		return deliveryWait("execution-review", fmt.Errorf("execute admitted delivery: %w", err))
	}
	content := ""
	if result.Response != nil {
		content = result.Response.Content
	}
	payload, err := json.Marshal(struct {
		AgentID string `json:"agent_id"`
		Content string `json:"content"`
	}{result.AgentID, content})
	if err != nil {
		return deliveryWait("execution-review", fmt.Errorf("encode delivery result: %w", err))
	}
	return execution.Outcome{
		Kind: execution.OutcomeWaiting, WaitState: execution.DeliveryWaitingDecision,
		Signal: "acceptance-verification", Result: payload,
	}
}

func deliveryWait(signal string, err error) execution.Outcome {
	return execution.Outcome{Kind: execution.OutcomeWaiting, WaitState: execution.DeliveryWaitingDecision, Signal: signal, Err: err}
}
