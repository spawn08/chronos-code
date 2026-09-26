package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/execution"
)

type deliveryUsageHook struct{}

// deliveryBudgetHook holds both reservations behind one SDK hook boundary.
// Uninstrumented SDK retries are disabled by the worker-owned context.
type deliveryBudgetHook struct {
	session budgetHook
	usage   deliveryUsageHook
}

func (h deliveryBudgetHook) Before(ctx context.Context, event *hooks.Event) error {
	if lease, ok := execution.OperationLeaseFromContext(ctx); ok && lease.Lease.Delivery.MaxCostMicrodollars > 0 && event.Type == hooks.EventModelCallBefore {
		request, ok := event.Input.(*model.ChatRequest)
		if !ok || request == nil {
			return execution.ErrInvalidDelivery
		}
		if request.MaxTokens == 0 {
			request.MaxTokens = 1024
		}
	}
	if err := h.session.Before(ctx, event); err != nil {
		return err
	}
	if err := h.usage.Before(ctx, event); err != nil {
		if event.Metadata != nil {
			if reserved, ok := event.Metadata[budgetReservationMetadataKey].(budgetReservation); ok {
				delete(event.Metadata, budgetReservationMetadataKey)
				return errors.Join(err, reserved.tracker.ReconcileUsage(reserved.id, model.Usage{}))
			}
		}
		return err
	}
	return nil
}

func (h deliveryBudgetHook) After(ctx context.Context, event *hooks.Event) error {
	return errors.Join(h.usage.After(ctx, event), h.session.After(ctx, event))
}

func (deliveryUsageHook) Before(ctx context.Context, event *hooks.Event) error {
	lease, ok := execution.OperationLeaseFromContext(ctx)
	if !ok || event.Type != hooks.EventModelCallBefore {
		return nil
	}
	request, ok := event.Input.(*model.ChatRequest)
	if !ok || event.Metadata == nil {
		return execution.ErrInvalidDelivery
	}
	provider, ok := event.Metadata["provider"].(model.Provider)
	if !ok || provider == nil {
		return execution.ErrInvalidDelivery
	}
	identity, ok := agent.RunIdentityFromContext(ctx)
	callSeq, _ := event.Metadata["correlation_id"].(string)
	if !ok || identity.InvocationID == "" || callSeq == "" {
		return execution.ErrInvalidDelivery
	}
	modelID := request.Model
	if modelID == "" {
		modelID = provider.Model()
	}
	input := tokenCounterForEvent(event, modelID).CountTokens(request.Messages)
	if input < 0 || request.MaxTokens > math.MaxInt-input {
		return execution.ErrInvalidDelivery
	}
	maximum := request.MaxTokens
	if maximum < 0 {
		return execution.ErrInvalidDelivery
	}
	if lease.Lease.Delivery.MaxCostMicrodollars > 0 && maximum == 0 {
		// A capped request must reserve a finite output allowance before the
		// provider sees it. Uncapped legacy requests retain their model default.
		maximum = 1024
		request.MaxTokens = maximum
	}
	if maximum > math.MaxInt-input {
		return execution.ErrInvalidDelivery
	}
	estimate := input + maximum
	price, priceErr := budget.PriceForModel(modelID)
	known := priceErr == nil
	if lease.Lease.Delivery.MaxCostMicrodollars > 0 && priceErr != nil {
		return execution.ErrUnknownPricing
	}
	var reserved int64
	if known {
		cost, err := price.ReserveCost(input, maximum)
		if err != nil {
			return fmt.Errorf("price delivery model call: %w", err)
		}
		reserved = int64(cost)
	}
	callID := identity.InvocationID + ":" + callSeq
	record, err := lease.Store.ReserveUsageLease(ctx, lease.Lease, execution.UsageReservation{
		CallID: callID, NodeID: identity.NodeID, Provider: provider.Name(), Model: modelID,
		EstimateTokens: int64(estimate), ReservedMicrodollars: reserved, KnownPrice: known,
	})
	if err != nil {
		return fmt.Errorf("reserve delivery model usage: %w", err)
	}
	if record.Status != "reserved" {
		return execution.ErrUsageOutcomeUnknown
	}
	event.Metadata["delivery_usage_call_id"] = callID
	event.Metadata["delivery_usage_started"] = time.Now()
	event.Metadata["delivery_usage_price"] = price
	event.Metadata["delivery_usage_price_known"] = known
	return nil
}

func (deliveryUsageHook) After(ctx context.Context, event *hooks.Event) error {
	lease, ok := execution.OperationLeaseFromContext(ctx)
	if !ok || event.Type != hooks.EventModelCallAfter {
		return nil
	}
	// The provider may have billed even when its worker context was canceled.
	// Persist the usage under a bounded worker-owned cleanup context.
	accountingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	callID, _ := event.Metadata["delivery_usage_call_id"].(string)
	started, _ := event.Metadata["delivery_usage_started"].(time.Time)
	if callID == "" || started.IsZero() {
		return execution.ErrInvalidDelivery
	}
	if attempts, ok := event.Metadata["provider_attempts"].(int); ok && attempts == 0 {
		return lease.Store.CancelUnusedReservation(accountingCtx, lease.Lease.Delivery.DeliveryScope, lease.Lease.Delivery.ID, callID)
	}
	response, _ := event.Output.(*model.ChatResponse)
	if response == nil || response.Usage == (model.Usage{}) && !response.UsageKnown {
		return lease.Store.MarkProviderUsageUnknownWithDuration(accountingCtx, lease.Lease.Delivery.DeliveryScope, lease.Lease.Delivery.ID, callID, time.Since(started).Nanoseconds())
	}
	actual := execution.IncurredUsage{
		InputTokens: int64(response.Usage.PromptTokens), OutputTokens: int64(response.Usage.CompletionTokens),
		CacheReadTokens: int64(response.Usage.CacheReadTokens), CacheCreationTokens: int64(response.Usage.CacheCreationTokens),
		ProviderNanoseconds: time.Since(started).Nanoseconds(),
	}
	known, _ := event.Metadata["delivery_usage_price_known"].(bool)
	if known {
		price, _ := event.Metadata["delivery_usage_price"].(budget.ModelPrice)
		cost, err := price.IncurredCost(response.Usage)
		if err != nil {
			_, recordErr := lease.Store.ReconcileUsage(accountingCtx, lease.Lease.Delivery.DeliveryScope, lease.Lease.Delivery.ID, callID, actual)
			return errors.Join(fmt.Errorf("price incurred model usage: %w", err), execution.ErrUsageOutcomeUnknown, recordErr)
		}
		actual.CostKnown, actual.CostMicrodollars = true, int64(cost)
	}
	_, err := lease.Store.ReconcileUsage(accountingCtx, lease.Lease.Delivery.DeliveryScope, lease.Lease.Delivery.ID, callID, actual)
	return err
}
