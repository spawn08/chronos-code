package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/security"
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
			return deliveryWait("effect-reconciliation", execution.ErrEffectNeedsReconciliation)
		}
	}
	if _, hasIntent, err := memory.ParseIntent(delivery.Goals[len(delivery.Goals)-1].Statement); err != nil || hasIntent {
		return deliveryWait("read-only-mode", fmt.Errorf("memory mutations are unavailable in read-only delivery: %w", execution.ErrInvalidDelivery))
	}
	ctx = authorization.WithRequest(ctx, request)
	ctx = executionAttempt.OperationContext(ctx)
	ctx = security.WithEffectGrant(ctx, security.EffectRead)
	ctx = security.WithMandatorySandbox(ctx, e.sandbox)
	result, err := e.orchestrator.Execute(ctx, ExecutionRequest{
		Message: delivery.Goals[len(delivery.Goals)-1].Statement,
		TaskID:  string(delivery.ID), SessionID: "delivery:" + string(delivery.ID), BoundedContext: true,
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
