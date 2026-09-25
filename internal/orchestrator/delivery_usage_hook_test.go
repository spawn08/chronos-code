package orchestrator

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/execution"
)

func TestDeliveryModelHookPersistsObservedAndUnknownUsage(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", Goal: execution.Goal{Statement: "inspect", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx = agent.WithRunIdentity(execution.WithOperationLease(ctx, store, lease), agent.RunIdentity{TaskID: "delivery", RoleID: "reader", InvocationID: "invocation"})
	hook := deliveryUsageHook{}
	provider := &executionTestProvider{name: "anthropic", modelID: "claude-sonnet-4-6"}
	event := &hooks.Event{Type: hooks.EventModelCallBefore, Input: &model.ChatRequest{Model: "claude-sonnet-4-6", Messages: []model.Message{{Role: model.RoleUser, Content: "inspect"}}, MaxTokens: 10}, Metadata: map[string]any{"provider": model.Provider(provider), "correlation_id": "call-1"}}
	if err := hook.Before(ctx, event); err != nil {
		t.Fatal(err)
	}
	reserved, err := store.Usage(ctx, scope, "delivery")
	if err != nil || reserved.OutstandingCalls != 1 || reserved.ReservedMicrodollars == 0 {
		t.Fatalf("pre-call reservation = %+v, error = %v", reserved, err)
	}
	event.Type, event.Output = hooks.EventModelCallAfter, &model.ChatResponse{Usage: model.Usage{PromptTokens: 5, CompletionTokens: 2}}
	if err := hook.After(ctx, event); err != nil {
		t.Fatal(err)
	}
	usage, err := store.Usage(ctx, scope, "delivery")
	if err != nil || usage.SpentMicrodollars != 45 || usage.InputTokens != 5 || usage.OutputTokens != 2 || usage.OutstandingCalls != 0 || !usage.CostKnown || usage.ActiveNanoseconds != 0 || usage.ProviderNanoseconds <= 0 {
		t.Fatalf("known priced usage = %+v, error = %v", usage, err)
	}
	unknownProvider := &executionTestProvider{name: "azure", modelID: "tenant-deployment"}
	unknown := &hooks.Event{Type: hooks.EventModelCallBefore, Input: &model.ChatRequest{Model: "tenant-deployment", Messages: []model.Message{{Role: model.RoleUser, Content: "inspect"}}}, Metadata: map[string]any{"provider": model.Provider(unknownProvider), "correlation_id": "call-2"}}
	if err := hook.Before(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	unknown.Type, unknown.Output = hooks.EventModelCallAfter, &model.ChatResponse{Usage: model.Usage{PromptTokens: 8, CompletionTokens: 3}}
	if err := hook.After(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	usage, err = store.Usage(ctx, scope, "delivery")
	if err != nil || usage.InputTokens != 13 || usage.OutputTokens != 5 || usage.CostKnown || usage.SpentMicrodollars != 45 {
		t.Fatalf("unknown-price usage = %+v, error = %v", usage, err)
	}
}

func TestDeliveryModelHookPreservesUnknownProviderOutcome(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", Goal: execution.Goal{Statement: "inspect", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx = agent.WithRunIdentity(execution.WithOperationLease(ctx, store, lease), agent.RunIdentity{TaskID: "delivery", RoleID: "reader", InvocationID: "invocation"})
	event := &hooks.Event{Type: hooks.EventModelCallBefore, Input: &model.ChatRequest{Model: "deployment"}, Metadata: map[string]any{"provider": model.Provider(&executionTestProvider{name: "azure", modelID: "deployment"}), "correlation_id": "failed"}}
	hook := deliveryUsageHook{}
	if err := hook.Before(ctx, event); err != nil {
		t.Fatal(err)
	}
	event.Type = hooks.EventModelCallAfter
	if err := hook.After(ctx, event); err != nil {
		t.Fatal(err)
	}
	usage, err := store.Usage(ctx, scope, "delivery")
	if err != nil || usage.UnknownCalls != 1 || usage.OutstandingCalls != 1 || usage.CostKnown || usage.ActiveNanoseconds != 0 || usage.ProviderNanoseconds <= 0 {
		t.Fatalf("provider failure accounting = %+v, error = %v", usage, err)
	}
}

func TestDeliveryModelHookCapsOutputBeforeReservationAndAcceptsReportedZero(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", MaxCostMicrodollars: 20000, Goal: execution.Goal{Statement: "inspect", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx = agent.WithRunIdentity(execution.WithOperationLease(ctx, store, lease), agent.RunIdentity{TaskID: "delivery", RoleID: "reader", InvocationID: "invocation"})
	request := &model.ChatRequest{Model: "claude-sonnet-4-6", Messages: []model.Message{{Role: model.RoleUser, Content: "inspect"}}}
	provider := &executionTestProvider{name: "anthropic", modelID: request.Model}
	event := &hooks.Event{Type: hooks.EventModelCallBefore, Input: request, Metadata: map[string]any{"provider": model.Provider(provider), "correlation_id": "call"}}
	hook := deliveryUsageHook{}
	if err := hook.Before(ctx, event); err != nil || request.MaxTokens != 1024 {
		t.Fatalf("bounded capped request = %+v, error = %v", request, err)
	}
	reserved, err := store.Usage(ctx, scope, "delivery")
	if err != nil || reserved.ReservedMicrodollars == 0 {
		t.Fatalf("capped reservation = %+v, error = %v", reserved, err)
	}
	event.Type, event.Output = hooks.EventModelCallAfter, &model.ChatResponse{UsageKnown: true}
	if err := hook.After(ctx, event); err != nil {
		t.Fatal(err)
	}
	usage, err := store.Usage(ctx, scope, "delivery")
	if err != nil || usage.OutstandingCalls != 0 || usage.UnknownCalls != 0 || usage.ReservedMicrodollars != 0 || !usage.CostKnown {
		t.Fatalf("explicit zero usage = %+v, error = %v", usage, err)
	}
	unsent := &hooks.Event{Type: hooks.EventModelCallBefore, Input: &model.ChatRequest{Model: provider.Model()}, Metadata: map[string]any{"provider": model.Provider(provider), "correlation_id": "unsent"}}
	if err := hook.Before(ctx, unsent); err != nil {
		t.Fatal(err)
	}
	unsent.Type = hooks.EventModelCallAfter
	unsent.Metadata["provider_attempts"] = 0
	if err := hook.After(ctx, unsent); err != nil {
		t.Fatal(err)
	}
	usage, err = store.Usage(ctx, scope, "delivery")
	if err != nil || usage.OutstandingCalls != 0 || usage.UnknownCalls != 0 || usage.ReservedMicrodollars != 0 {
		t.Fatalf("unsent reservation was not refunded = %+v, error = %v", usage, err)
	}
}

func TestCappedDeliveryAdmissionRefundsSessionReservationBeforeProviderCall(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", MaxCostMicrodollars: 1, Goal: execution.Goal{Statement: "inspect", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx = agent.WithRunIdentity(execution.WithOperationLease(ctx, store, lease), agent.RunIdentity{TaskID: "delivery", RoleID: "reader", InvocationID: "invocation"})
	tracker := budget.NewTracker(0, 0)
	hook := deliveryBudgetHook{session: budgetHook{tracker: tracker, orchestrator: &Orchestrator{}, agentID: "reader"}}
	provider := &executionTestProvider{name: "anthropic", modelID: "claude-sonnet-4-6"}
	event := &hooks.Event{Type: hooks.EventModelCallBefore, Input: &model.ChatRequest{Model: provider.Model(), Messages: []model.Message{{Role: model.RoleUser, Content: "inspect"}}}, Metadata: map[string]any{"provider": model.Provider(provider), "correlation_id": "first"}}
	if err := hook.Before(ctx, event); !errors.Is(err, execution.ErrCostAuthority) {
		t.Fatalf("capped provider admission = %v", err)
	}
	if cost := hook.session.orchestrator.currentUSDBudget().Cost("reader"); cost.ReservedMicrodollars != 0 {
		t.Fatalf("stranded session cost reservation = %+v", cost)
	}
	usage, err := store.Usage(ctx, scope, "delivery")
	if err != nil || usage.OutstandingCalls != 0 || usage.ReservedMicrodollars != 0 {
		t.Fatalf("stranded delivery cost reservation = %+v, error = %v", usage, err)
	}
}

func TestDeliveryModelHookPersistsBilledUsageAfterWorkerCancellation(t *testing.T) {
	base := context.Background()
	store, err := execution.OpenDeliveryStore(base, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(base, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", Goal: execution.Goal{Statement: "inspect", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(base, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(base)
	ctx = agent.WithRunIdentity(execution.WithOperationLease(ctx, store, lease), agent.RunIdentity{TaskID: "delivery", RoleID: "reader", InvocationID: "worker"})
	provider := &executionTestProvider{name: "anthropic", modelID: "claude-sonnet-4-6"}
	event := &hooks.Event{Type: hooks.EventModelCallBefore, Input: &model.ChatRequest{Model: provider.Model(), Messages: []model.Message{{Role: model.RoleUser, Content: "inspect"}}}, Metadata: map[string]any{"provider": model.Provider(provider), "correlation_id": "call"}}
	hook := deliveryUsageHook{}
	if err := hook.Before(ctx, event); err != nil {
		t.Fatal(err)
	}
	cancel()
	event.Type, event.Output = hooks.EventModelCallAfter, &model.ChatResponse{UsageKnown: true, Usage: model.Usage{PromptTokens: 3, CompletionTokens: 2}}
	if err := hook.After(ctx, event); err != nil {
		t.Fatalf("account for canceled provider call: %v", err)
	}
	usage, err := store.Usage(base, scope, "delivery")
	if err != nil || usage.InputTokens != 3 || usage.OutputTokens != 2 || usage.OutstandingCalls != 0 || !usage.CostKnown {
		t.Fatalf("cancelled incurred usage = %+v, error = %v", usage, err)
	}
}

func TestDeliveryModelHookKeepsObservedTokensWhenPricingOverflows(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", MaxCostMicrodollars: 20000, Goal: execution.Goal{Statement: "inspect", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx = agent.WithRunIdentity(execution.WithOperationLease(ctx, store, lease), agent.RunIdentity{TaskID: "delivery", RoleID: "reader", InvocationID: "worker"})
	provider := &executionTestProvider{name: "anthropic", modelID: "claude-sonnet-4-6"}
	event := &hooks.Event{Type: hooks.EventModelCallBefore, Input: &model.ChatRequest{Model: provider.Model(), Messages: []model.Message{{Role: model.RoleUser, Content: "inspect"}}}, Metadata: map[string]any{"provider": model.Provider(provider), "correlation_id": "call"}}
	hook := deliveryUsageHook{}
	if err := hook.Before(ctx, event); err != nil {
		t.Fatal(err)
	}
	event.Type, event.Output = hooks.EventModelCallAfter, &model.ChatResponse{UsageKnown: true, Usage: model.Usage{PromptTokens: math.MaxInt, CompletionTokens: math.MaxInt}}
	if err := hook.After(ctx, event); !errors.Is(err, execution.ErrUsageOutcomeUnknown) {
		t.Fatalf("overflowing model price = %v", err)
	}
	usage, err := store.Usage(ctx, scope, "delivery")
	if err != nil || usage.InputTokens != math.MaxInt || usage.OutputTokens != math.MaxInt || usage.CostKnown || usage.OutstandingCalls != 0 {
		t.Fatalf("retained unpriced actual usage = %+v, error = %v", usage, err)
	}
}

func TestDurableModelWindowCannotSilentlyResetSpentTokens(t *testing.T) {
	tracker := budget.NewTracker(1, 0)
	ctx := storage.WithSession(context.Background(), "delivery")
	if err := tracker.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter, Output: &model.ChatResponse{Usage: model.Usage{PromptTokens: 2}}}); err != nil {
		t.Fatal(err)
	}
	ctx = execution.WithOperationLease(ctx, &execution.DeliveryStore{}, execution.Lease{Delivery: execution.Delivery{ID: "delivery"}})
	err := (budgetHook{tracker: tracker}).Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore})
	if err == nil || tracker.Ratio("delivery") != 2 {
		t.Fatalf("durable window reset instead of stopping: ratio=%v error=%v", tracker.Ratio("delivery"), err)
	}
}
