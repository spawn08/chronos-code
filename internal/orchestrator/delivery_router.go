package orchestrator

import (
	"context"
	"fmt"

	"github.com/spawn08/chronos-code/internal/execution"
)

// RoutedDeliveryExecutor lets one worker service run every admitted delivery
// kind. The route comes from the persisted admission policy, never from goal
// text or model output, and each capability reflects only its own executor.
type RoutedDeliveryExecutor struct {
	readOnly *ReadOnlyDeliveryExecutor
	plan     *PlanDeliveryExecutor
}

func NewRoutedDeliveryExecutor(readOnly *ReadOnlyDeliveryExecutor, planExecutor *PlanDeliveryExecutor) (*RoutedDeliveryExecutor, error) {
	if readOnly == nil && planExecutor == nil {
		return nil, execution.ErrInvalidDelivery
	}
	return &RoutedDeliveryExecutor{readOnly: readOnly, plan: planExecutor}, nil
}

func (e *RoutedDeliveryExecutor) SupportsCappedDelivery() bool {
	return e != nil && e.readOnly != nil && e.readOnly.SupportsCappedDelivery()
}

func (e *RoutedDeliveryExecutor) SupportsCheckpointedTeam() bool {
	return e != nil && e.readOnly != nil && e.readOnly.SupportsCheckpointedTeam()
}

func (e *RoutedDeliveryExecutor) SupportsPlanDelivery() bool {
	return e != nil && e.plan != nil && e.plan.SupportsPlanDelivery()
}

func (e *RoutedDeliveryExecutor) SupportsCandidatePlanDelivery() bool {
	return e != nil && e.plan != nil && e.plan.SupportsCandidatePlanDelivery()
}

// PlanPolicyReference is the policy stamped on plan admissions this worker
// can execute, or empty when no plan executor is installed.
func (e *RoutedDeliveryExecutor) PlanPolicyReference() string {
	if e == nil || e.plan == nil {
		return ""
	}
	return e.plan.PolicyReference()
}

func (e *RoutedDeliveryExecutor) Execute(ctx context.Context, attempt *execution.Execution) execution.Outcome {
	switch attempt.Lease.Delivery.PolicyReference {
	case execution.InternalPlanPolicyReference, execution.CandidatePlanPolicyReference:
		if e.plan == nil {
			return deliveryWait("executor-unavailable", fmt.Errorf("no plan executor installed: %w", execution.ErrInvalidDelivery))
		}
		return e.plan.Execute(ctx, attempt)
	default:
		if e.readOnly == nil {
			return deliveryWait("executor-unavailable", fmt.Errorf("no read-only executor installed: %w", execution.ErrInvalidDelivery))
		}
		return e.readOnly.Execute(ctx, attempt)
	}
}
