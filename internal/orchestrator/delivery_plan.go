package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

var ErrPlanHandoffConflict = errors.New("plan generation conflicts with persisted proposal")

// PrepareDeliveryPlan validates and persists a host-scoped generation before
// admission is made runnable. The plan and delivery use separate databases:
// if queue promotion fails, retrying the same proposal completes the handoff;
// a different proposal for the same identity is rejected.
func (o *Orchestrator) PrepareDeliveryPlan(ctx context.Context, strategistJSON []byte, identity PlanRuntimeIdentity) error {
	if o == nil || !o.closedLoopPPDEnabled() || o.planStore == nil || o.planController == nil {
		return execution.ErrInvalidDelivery
	}
	output, err := plan.ParseStrategistOutput(strategistJSON)
	if err != nil {
		return err
	}
	requested := plan.DecompositionRequest{
		TenantID: identity.TenantID, RepositoryID: identity.RepositoryID, TaskID: identity.TaskID,
		PlanID: identity.PlanID, Generation: identity.Generation,
		SourceRequestRef: output.SourceRequestRef, ClassifierRef: output.ClassifierRef, Nodes: output.Nodes,
	}
	ref := plan.Plan{TenantID: identity.TenantID, RepositoryID: identity.RepositoryID, TaskID: identity.TaskID, ID: identity.PlanID, Generation: identity.Generation}
	stored, err := o.planStore.Load(ctx, ref)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = o.planController.Decompose(ctx, requested)
		return err
	}
	if err != nil {
		return err
	}
	if !samePlanProposal(stored, requested) {
		return ErrPlanHandoffConflict
	}
	return o.validatePlanArtifactReceipts(stored)
}

// PlanDeliveryExecutor is an opt-in worker adapter for a host-persisted plan
// generation. The public handoff requires an explicitly installed plan worker.
type PlanDeliveryExecutor struct {
	orchestrator *Orchestrator
	authorizer   authorization.Authorizer
	sandbox      security.SandboxPolicy
}

func NewPlanDeliveryExecutor(orch *Orchestrator, authorizer authorization.Authorizer, sandbox security.SandboxPolicy) (*PlanDeliveryExecutor, error) {
	if orch == nil || authorizer == nil || orch.planStore == nil || orch.planController == nil || !orch.closedLoopPPDEnabled() {
		return nil, execution.ErrInvalidDelivery
	}
	return &PlanDeliveryExecutor{orchestrator: orch, authorizer: authorizer, sandbox: sandbox}, nil
}

func (e *PlanDeliveryExecutor) SupportsPlanDelivery() bool { return e != nil }

func (e *PlanDeliveryExecutor) Execute(ctx context.Context, attempt *execution.Execution) execution.Outcome {
	d := attempt.Lease.Delivery
	if d.PolicyReference != execution.InternalPlanPolicyReference || d.ID == "" || d.State != execution.DeliveryRunning {
		return deliveryWait("invalid-delivery-plan", execution.ErrInvalidDelivery)
	}
	authority := authorization.Request{PrincipalID: "delivery-plan-worker", TenantID: string(d.TenantID), RepositoryID: string(d.RepositoryID), Action: "delivery.execute"}
	if err := e.authorizer.Authorize(ctx, authority); err != nil {
		return deliveryWait("authorization-required", fmt.Errorf("authorize plan worker: %w", err))
	}
	ref := plan.Plan{TenantID: plan.TenantID(d.TenantID), RepositoryID: plan.RepositoryID(d.RepositoryID), TaskID: plan.TaskID(d.ID), ID: plan.PlanID(d.ID), Generation: plan.GenerationID(strconv.FormatInt(int64(d.CurrentGoalRevision), 10))}
	persisted, err := e.orchestrator.planStore.Load(ctx, ref)
	if errors.Is(err, sql.ErrNoRows) {
		return deliveryWait("plan-not-admitted", err)
	}
	if err != nil {
		return deliveryWait("plan-review", fmt.Errorf("load admitted plan: %w", err))
	}
	if err := e.orchestrator.validatePlanArtifactReceipts(persisted); err != nil {
		return deliveryWait("artifact-reconciliation", err)
	}
	operations, err := attempt.PriorOperations(ctx)
	if err != nil {
		return deliveryWait("effect-reconciliation", err)
	}
	completed := make(map[string]bool, len(persisted.Nodes))
	for _, node := range persisted.Nodes {
		completed[string(node.ID)] = node.State == plan.NodeCompleted
	}
	for _, op := range operations {
		if op.Attempt != attempt.Lease.Attempt && (op.NodeID == "" || !completed[op.NodeID] || op.Status != execution.OperationObserved && op.Status != execution.OperationReconciled) {
			return deliveryWait("effect-reconciliation", execution.ErrEffectNeedsReconciliation)
		}
	}
	usage, err := attempt.CumulativeUsage(ctx)
	if err != nil || usage.OutstandingCalls > 0 {
		return deliveryWait("usage-reconciliation", errors.Join(err, execution.ErrUsageOutcomeUnknown))
	}
	ctx = authorization.WithRequest(ctx, authority)
	ctx = storage.WithSession(ctx, "delivery:"+string(d.ID))
	ctx = agent.WithRunIdentity(ctx, agent.RunIdentity{
		TenantID: string(d.TenantID), RepositoryID: string(d.RepositoryID), DeliveryID: string(d.ID), TaskID: string(d.ID),
		RoleID: "delivery-plan-worker", InvocationID: fmt.Sprintf("delivery:%s:%d", d.ID, attempt.Lease.Epoch),
		AttemptID: strconv.FormatInt(attempt.Lease.Attempt, 10), GoalRevision: strconv.FormatInt(int64(d.CurrentGoalRevision), 10),
		SessionID: "delivery:" + string(d.ID), PolicyRevision: d.PolicyReference,
	})
	ctx = attempt.OperationContext(ctx)
	ctx = plan.WithAdmissionGuard(ctx, func(ctx context.Context, claim func(context.Context) error) error {
		return attempt.WithLeaseEffect(ctx, 2*time.Minute, claim)
	})
	ctx = agent.WithModelRetriesDisabled(ctx)
	ctx = security.WithEffectGrant(ctx, security.EffectRead, security.EffectScratchWrite, security.EffectDeliveryWrite,
		security.EffectProcessExecution, security.EffectNetwork, security.EffectExternalMutation)
	ctx = security.WithMandatorySandbox(ctx, e.sandbox)
	finished, err := e.orchestrator.planController.Run(ctx, persisted)
	if err != nil {
		return deliveryWait("plan-review", fmt.Errorf("run admitted plan: %w", err))
	}
	if finished.State != plan.PlanCompleted {
		return deliveryWait("plan-review", fmt.Errorf("admitted plan stopped in state %q (%s)", finished.State, finished.StopReason))
	}
	payload, err := json.Marshal(struct {
		PlanID     plan.PlanID       `json:"plan_id"`
		Generation plan.GenerationID `json:"generation"`
		Artifacts  []plan.Artifact   `json:"artifacts"`
	}{finished.ID, finished.Generation, finished.Artifacts})
	if err != nil {
		return deliveryWait("plan-review", fmt.Errorf("encode plan result: %w", err))
	}
	return execution.Outcome{Kind: execution.OutcomeWaiting, WaitState: execution.DeliveryWaitingDecision, Signal: "acceptance-verification", Result: payload}
}
