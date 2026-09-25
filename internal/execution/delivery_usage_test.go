package execution

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDeliveryUsageRecordsActualOverageOnceAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store := openTestDeliveryStore(t, path)
	admission := testAdmission("tenant", "repo", "delivery", "key")
	admission.MaxCostMicrodollars = 10
	if _, err := store.Admit(ctx, admission); err != nil {
		t.Fatal(err)
	}
	first := UsageReservation{CallID: "call-1", Provider: "anthropic", Model: "sonnet", EstimateTokens: 3, ReservedMicrodollars: 6, KnownPrice: true}
	if _, err := store.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, first); err != nil {
		t.Fatal(err)
	}
	other := first
	other.CallID = "call-2"
	if _, err := store.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, other); !errors.Is(err, ErrCostAuthority) {
		t.Fatalf("overlapping reservation = %v", err)
	}
	actual := IncurredUsage{InputTokens: 4, OutputTokens: 2, CacheReadTokens: 1, CostMicrodollars: 12, CostKnown: true, ActiveNanoseconds: 1000}
	usage, err := store.ReconcileUsage(ctx, admission.Scope, admission.DeliveryID, first.CallID, actual)
	if !errors.Is(err, ErrCostAuthority) || usage.SpentMicrodollars != 12 || usage.ReservedMicrodollars != 0 {
		t.Fatalf("incurred overage = %+v, error = %v", usage, err)
	}
	if _, err := store.ReconcileUsage(ctx, admission.Scope, admission.DeliveryID, first.CallID, actual); !errors.Is(err, ErrCostAuthority) {
		t.Fatalf("duplicate overage completion = %v", err)
	}
	if _, err := store.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, other); !errors.Is(err, ErrCostAuthority) {
		t.Fatalf("new spend after overage = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestDeliveryStore(t, path)
	usage, err = store.Usage(ctx, admission.Scope, admission.DeliveryID)
	if err != nil || usage.SpentMicrodollars != 12 || usage.InputTokens != 4 || usage.OutputTokens != 2 || usage.CacheReadTokens != 1 || usage.ActiveNanoseconds != 1000 || !usage.CostKnown {
		t.Fatalf("usage after restart = %+v, error = %v", usage, err)
	}
	if _, err := store.Usage(ctx, DeliveryScope{TenantID: "other", RepositoryID: "repo"}, admission.DeliveryID); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("cross-tenant guessed usage = %v", err)
	}
}

func TestDeliveryUsageUnknownPricingAndCancellation(t *testing.T) {
	ctx := context.Background()
	store := openTestDeliveryStore(t, filepath.Join(t.TempDir(), "deliveries.db"))
	admission := testAdmission("tenant", "repo", "delivery", "key")
	admission.MaxCostMicrodollars = 100
	if _, err := store.Admit(ctx, admission); err != nil {
		t.Fatal(err)
	}
	unknown := UsageReservation{CallID: "unknown", Provider: "azure", Model: "deployment", EstimateTokens: 10}
	if _, err := store.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, unknown); !errors.Is(err, ErrUnknownPricing) {
		t.Fatalf("unpriced capped call = %v", err)
	}
	known := UsageReservation{CallID: "known", Provider: "anthropic", Model: "sonnet", EstimateTokens: 10, ReservedMicrodollars: 40, KnownPrice: true}
	if _, err := store.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, known); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUsageUnknownWithDuration(ctx, admission.Scope, admission.DeliveryID, known.CallID, 1000); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUsageUnknownWithDuration(ctx, admission.Scope, admission.DeliveryID, known.CallID, 9999); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, UsageReservation{CallID: "next", Provider: "anthropic", Model: "sonnet", EstimateTokens: 1, ReservedMicrodollars: 1, KnownPrice: true}); !errors.Is(err, ErrUsageOutcomeUnknown) {
		t.Fatalf("capped work while call outcome unknown = %v", err)
	}
	if err := store.CancelUnusedReservation(ctx, admission.Scope, admission.DeliveryID, known.CallID); !errors.Is(err, ErrUsageConflict) {
		t.Fatalf("unknown call reservation refunded = %v", err)
	}
	usage, err := store.Usage(ctx, admission.Scope, admission.DeliveryID)
	if err != nil || usage.UnknownCalls != 1 || usage.ReservedMicrodollars != 40 || usage.CostKnown || usage.ActiveNanoseconds != 1000 {
		t.Fatalf("unknown usage = %+v, error = %v", usage, err)
	}
	if _, err := store.ReconcileUsage(ctx, admission.Scope, admission.DeliveryID, known.CallID, IncurredUsage{InputTokens: 10, CostMicrodollars: 32, CostKnown: true, ActiveNanoseconds: 2000}); err != nil {
		t.Fatal(err)
	}
	if err := store.CancelUnusedReservation(ctx, admission.Scope, admission.DeliveryID, known.CallID); !errors.Is(err, ErrUsageConflict) {
		t.Fatalf("reconciled call refunded = %v", err)
	}
	if _, err := store.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, UsageReservation{CallID: "unsent", Provider: "anthropic", Model: "sonnet", ReservedMicrodollars: 20, KnownPrice: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.CancelUnusedReservation(ctx, admission.Scope, admission.DeliveryID, "unsent"); err != nil {
		t.Fatal(err)
	}
	usage, err = store.Usage(ctx, admission.Scope, admission.DeliveryID)
	if err != nil || usage.SpentMicrodollars != 32 || usage.ReservedMicrodollars != 0 || usage.UnknownCalls != 0 {
		t.Fatalf("refunded usage = %+v, error = %v", usage, err)
	}
}

func TestDeliveryUsageConcurrentReservationsCannotSpendSameCap(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store := openTestDeliveryStore(t, path)
	other := openTestDeliveryStore(t, path)
	admission := testAdmission("tenant", "repo", "delivery", "key")
	admission.MaxCostMicrodollars = 10
	if _, err := store.Admit(ctx, admission); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, target := range []*DeliveryStore{store, other} {
		wg.Add(1)
		go func(i int, target *DeliveryStore) {
			defer wg.Done()
			_, err := target.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, UsageReservation{CallID: "call-" + string(rune('a'+i)), Provider: "anthropic", Model: "sonnet", EstimateTokens: 5, ReservedMicrodollars: 8, KnownPrice: true})
			results <- err
		}(i, target)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrCostAuthority) && !strings.Contains(err.Error(), "database is locked") {
			t.Fatal(err)
		}
	}
	usage, err := store.Usage(ctx, admission.Scope, admission.DeliveryID)
	if err != nil || success != 1 || usage.ReservedMicrodollars != 8 {
		t.Fatalf("concurrent admissions = %d, usage = %+v, error = %v", success, usage, err)
	}
}

func TestDeliveryUsageRejectsOverflowBeforePersisting(t *testing.T) {
	ctx := context.Background()
	store := openTestDeliveryStore(t, filepath.Join(t.TempDir(), "deliveries.db"))
	admission := testAdmission("tenant", "repo", "delivery", "key")
	if _, err := store.Admit(ctx, admission); err != nil {
		t.Fatal(err)
	}
	for _, call := range []UsageReservation{{CallID: "max", Provider: "p", Model: "m", ReservedMicrodollars: math.MaxInt64, KnownPrice: true}, {CallID: "overflow", Provider: "p", Model: "m", ReservedMicrodollars: 1, KnownPrice: true}} {
		_, err := store.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, call)
		if call.CallID == "max" && err != nil || call.CallID == "overflow" && err == nil {
			t.Fatalf("call %s overflow error = %v", call.CallID, err)
		}
	}
}

func TestDeliveryUsagePersistsIncurredCallEvenWhenTotalOverflows(t *testing.T) {
	ctx := context.Background()
	store := openTestDeliveryStore(t, filepath.Join(t.TempDir(), "deliveries.db"))
	admission := testAdmission("tenant", "repo", "delivery", "key")
	if _, err := store.Admit(ctx, admission); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		if _, err := store.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, UsageReservation{CallID: id, Provider: "p", Model: "m", KnownPrice: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ReconcileUsage(ctx, admission.Scope, admission.DeliveryID, "first", IncurredUsage{CostKnown: true, CostMicrodollars: math.MaxInt64}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileUsage(ctx, admission.Scope, admission.DeliveryID, "second", IncurredUsage{CostKnown: true, CostMicrodollars: 1}); !errors.Is(err, ErrUsageOverflow) {
		t.Fatalf("overflowing incurred call = %v", err)
	}
	var recorded int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_usage_calls WHERE status = 'reconciled'`).Scan(&recorded); err != nil || recorded != 2 {
		t.Fatalf("incurred records = %d, error = %v", recorded, err)
	}
	if _, err := store.Usage(ctx, admission.Scope, admission.DeliveryID); !errors.Is(err, ErrUsageOverflow) {
		t.Fatalf("overflow presented as a known total: %v", err)
	}
}

func TestDeliveryUsagePersistsUnknownActualUnderCapAndBlocksFurtherSpend(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store := openTestDeliveryStore(t, path)
	admission := testAdmission("tenant", "repo", "delivery", "key")
	admission.MaxCostMicrodollars = 100
	if _, err := store.Admit(ctx, admission); err != nil {
		t.Fatal(err)
	}
	reservation := UsageReservation{CallID: "call", Provider: "p", Model: "m", EstimateTokens: 10, ReservedMicrodollars: 40, KnownPrice: true}
	if _, err := store.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, reservation); err != nil {
		t.Fatal(err)
	}
	actual := IncurredUsage{InputTokens: 8, OutputTokens: 4, ActiveNanoseconds: 123}
	for range 2 {
		usage, err := store.ReconcileUsage(ctx, admission.Scope, admission.DeliveryID, reservation.CallID, actual)
		if !errors.Is(err, ErrUnknownPricing) || usage.InputTokens != 8 || usage.OutputTokens != 4 || usage.ActiveNanoseconds != 123 || usage.ReservedMicrodollars != 0 || usage.CostKnown {
			t.Fatalf("unknown incurred usage = %+v, error = %v", usage, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestDeliveryStore(t, path)
	if _, err := store.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, UsageReservation{CallID: "next", Provider: "p", Model: "m", KnownPrice: true, ReservedMicrodollars: 1}); !errors.Is(err, ErrUsageOutcomeUnknown) {
		t.Fatalf("admit spend against unknown total = %v", err)
	}
	usage, err := store.Usage(ctx, admission.Scope, admission.DeliveryID)
	if err != nil || usage.InputTokens != 8 || usage.OutputTokens != 4 || usage.ActiveNanoseconds != 123 || usage.CostKnown {
		t.Fatalf("restarted usage = %+v, error = %v", usage, err)
	}
}

func TestDeliveryUsageSeparatesProviderWaitFromActiveComputeAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store := openTestDeliveryStore(t, path)
	admission := testAdmission("tenant", "repo", "delivery", "key")
	if _, err := store.Admit(ctx, admission); err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"observed", "unknown"} {
		if _, err := store.ReserveUsage(ctx, admission.Scope, admission.DeliveryID, UsageReservation{CallID: call, Provider: "p", Model: "m", KnownPrice: true}); err != nil {
			t.Fatal(err)
		}
	}
	actual := IncurredUsage{InputTokens: 2, CostKnown: true, ActiveNanoseconds: 1000, ProviderNanoseconds: 2000}
	if _, err := store.ReconcileUsage(ctx, admission.Scope, admission.DeliveryID, "observed", actual); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProviderUsageUnknownWithDuration(ctx, admission.Scope, admission.DeliveryID, "unknown", 3000); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProviderUsageUnknownWithDuration(ctx, admission.Scope, admission.DeliveryID, "unknown", 9999); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestDeliveryStore(t, path)
	usage, err := store.Usage(ctx, admission.Scope, admission.DeliveryID)
	if err != nil || usage.ActiveNanoseconds != 1000 || usage.ProviderNanoseconds != 5000 || usage.UnknownCalls != 1 {
		t.Fatalf("separated cumulative time = %+v, error = %v", usage, err)
	}
	if _, err := store.ReconcileUsage(ctx, admission.Scope, admission.DeliveryID, "observed", actual); err != nil {
		t.Fatal(err)
	}
	usage, err = store.Usage(ctx, admission.Scope, admission.DeliveryID)
	if err != nil || usage.ProviderNanoseconds != 5000 {
		t.Fatalf("duplicate completion changed wait time = %+v, error = %v", usage, err)
	}
}
