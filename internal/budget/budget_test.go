package budget

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/storage"
)

func TestBundledModelPricing(t *testing.T) {
	tests := []struct {
		model string
		want  Rates
	}{
		{model: "claude-haiku-4-5", want: Rates{1_000_000, 5_000_000, 100_000, 1_250_000, 2_000_000}},
		{model: "claude-sonnet-4-6", want: Rates{3_000_000, 15_000_000, 300_000, 3_750_000, 6_000_000}},
		{model: "claude-sonnet-5", want: Rates{2_000_000, 10_000_000, 200_000, 2_500_000, 4_000_000}},
		{model: "claude-opus-4-8", want: Rates{5_000_000, 25_000_000, 500_000, 6_250_000, 10_000_000}},
		// Sub-dollar rates must be exact; cache writes default to input.
		{model: "gpt-5-nano", want: Rates{50_000, 400_000, 5_000, 50_000, 50_000}},
		// Omitted cache rates default to the input rate.
		{model: "mistral-large-latest", want: Rates{500_000, 1_500_000, 500_000, 500_000, 500_000}},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got, err := PriceForModel(tt.model)
			if err != nil {
				t.Fatalf("PriceForModel() error = %v", err)
			}
			if got.Rates != tt.want {
				t.Errorf("PriceForModel() = %+v, want %+v", got.Rates, tt.want)
			}
		})
	}

	if _, err := PriceForModel("unpriced-model"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("PriceForModel(unknown) error = %v, want ErrUnknownModel", err)
	}
}

// Every model the bundled routing table or /model picker offers by default
// must be priced, or the TUI can only report its cost as unpriced.
func TestDefaultRoutedModelsArePriced(t *testing.T) {
	for _, id := range []string{"claude-haiku-4-5", "claude-sonnet-4-6", "claude-sonnet-5", "claude-opus-4-8",
		"claude-opus-4-7", "claude-fable-5", "claude-sonnet-4-5", "gpt-5", "gpt-5-mini", "gpt-4o", "gpt-4o-mini", "o3"} {
		if _, err := PriceForModel(id); err != nil {
			t.Errorf("PriceForModel(%q) error = %v", id, err)
		}
	}
}

func TestPriceForModelNormalizesProviderForms(t *testing.T) {
	want, err := PriceForModel("claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("PriceForModel() error = %v", err)
	}
	for _, id := range []string{
		"Claude-Sonnet-4-5",
		"anthropic/claude-sonnet-4-5",
		"anthropic/claude-sonnet-4.5",
		"claude-sonnet-4-5-20250929",
		"claude-sonnet-4-5@20250929",
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	} {
		got, err := PriceForModel(id)
		if err != nil || got.Rates != want.Rates {
			t.Errorf("PriceForModel(%q) = %+v, %v; want %+v", id, got, err, want)
		}
	}
	// Dotted IDs that are themselves entries must not be dash-normalized.
	if got, err := PriceForModel("openai/gpt-5.5"); err != nil || got.InputPerMillion != 5_000_000 {
		t.Errorf("PriceForModel(openai/gpt-5.5) = %+v, %v", got, err)
	}
}

func TestLoadPricingOverlaysMergeAndValidate(t *testing.T) {
	t.Cleanup(func() {
		if err := LoadPricing(); err != nil {
			t.Fatalf("reset pricing: %v", err)
		}
	})
	err := LoadPricing(
		PricingOverlay{Source: "user", Data: []byte("models:\n  my-local-model: { input: 0.1, output: 0.2 }\n  claude-sonnet-5: { input: 9, output: 9 }\n")},
		PricingOverlay{Source: "project", Data: []byte("models:\n  Claude-Sonnet-5: { input: 4, output: 8, cache_read: 0.4 }\n")},
	)
	if err != nil {
		t.Fatalf("LoadPricing() error = %v", err)
	}
	if got, _ := PriceForModel("claude-sonnet-5"); got.Rates != (Rates{4_000_000, 8_000_000, 400_000, 4_000_000, 4_000_000}) {
		t.Errorf("project overlay did not win: %+v", got)
	}
	if got, err := PriceForModel("my-local-model"); err != nil || got.InputPerMillion != 100_000 {
		t.Errorf("user overlay model = %+v, %v", got, err)
	}
	if _, err := PriceForModel("claude-haiku-4-5"); err != nil {
		t.Errorf("bundled entry lost after overlay: %v", err)
	}

	for _, bad := range []string{
		"models:\n  x: { output: 1 }\n",
		"models:\n  x: { input: -1, output: 1 }\n",
		"models:\n  x: { input: 0, output: 1 }\n",
		"models: [",
		"models:\n  x: { input: 1, output: 1, tiers: [{ above: 0, input: 2, output: 2 }] }\n",
		"models:\n  x: { input: 1, output: 1, tiers: [{ above: 9, input: 2, output: 2 }, { above: 5, input: 3, output: 3 }] }\n",
		"models:\n  x: { input: 1, output: 1, tiers: [{ above: 9, output: 2 }] }\n",
	} {
		if err := LoadPricing(PricingOverlay{Source: "project", Data: []byte(bad)}); err == nil {
			t.Errorf("LoadPricing(%q) error = nil, want validation error", bad)
		}
	}
	// A rejected overlay leaves the previously loaded table active.
	if got, _ := PriceForModel("claude-sonnet-5"); got.InputPerMillion != 4_000_000 {
		t.Errorf("failed LoadPricing replaced the active table: %+v", got)
	}
}

func TestIncurredCostUsesModelCacheRatesAndRounds(t *testing.T) {
	price, err := PriceForModel("gpt-4o")
	if err != nil {
		t.Fatalf("PriceForModel() error = %v", err)
	}
	// OpenAI-style: 10_000 prompt tokens of which 8_000 cached.
	// 2000*2.5 + 8000*1.25 + 100*10 = 5000 + 10000 + 1000 = 16000 µ$.
	got, err := price.IncurredCost(model.Usage{PromptTokens: 10_000, CacheReadTokens: 8_000, CompletionTokens: 100, CacheReadInPrompt: true})
	if err != nil || got != 16_000 {
		t.Fatalf("IncurredCost() = %d, %v; want 16000", got, err)
	}
	nano, _ := PriceForModel("gpt-5-nano")
	// 3 tokens * $0.05/M = 0.15 µ$ rounds to 0; a reservation rounds up to 1.
	if got, _ := nano.IncurredCost(model.Usage{PromptTokens: 3}); got != 0 {
		t.Errorf("IncurredCost(3 nano tokens) = %d, want 0", got)
	}
	if got, _ := nano.ReserveCost(3, 0); got != 1 {
		t.Errorf("ReserveCost(3 nano tokens) = %d, want 1", got)
	}
}

func TestLongContextTierAppliesToWholeCall(t *testing.T) {
	price, err := PriceForModel("gpt-5.5")
	if err != nil {
		t.Fatalf("PriceForModel() error = %v", err)
	}
	// At the threshold: base rates. 272000*5 + 1000*30 = 1_390_000 µ$.
	if got, _ := price.IncurredCost(model.Usage{PromptTokens: 272_000, CompletionTokens: 1_000, CacheReadInPrompt: true}); got != 1_390_000 {
		t.Errorf("IncurredCost(at threshold) = %d, want 1390000", got)
	}
	// One token over, counting cache hits: every token at tier rates.
	// uncached 72001*10 + cached 200000*1 + out 1000*45 = 720010 + 200000 + 45000.
	usage := model.Usage{PromptTokens: 272_001, CacheReadTokens: 200_000, CompletionTokens: 1_000, CacheReadInPrompt: true}
	if got, _ := price.IncurredCost(usage); got != 965_010 {
		t.Errorf("IncurredCost(over threshold) = %d, want 965010", got)
	}
	// Reservations pick the tier from the estimated input.
	if got, _ := price.ReserveCost(300_000, 0); got != 3_000_000 {
		t.Errorf("ReserveCost(over threshold) = %d, want 3000000", got)
	}
}

func TestOneHourCacheWritesUseTheirOwnRate(t *testing.T) {
	price, err := PriceForModel("claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("PriceForModel() error = %v", err)
	}
	// 1000 writes, 400 of them 1h: 600*3.75 + 400*6 = 2250 + 2400 = 4650 µ$.
	got, err := price.IncurredCost(model.Usage{CacheCreationTokens: 1_000, CacheCreation1hTokens: 400})
	if err != nil || got != 4_650 {
		t.Fatalf("IncurredCost() = %d, %v; want 4650", got, err)
	}
	// A 1h count larger than total writes is clamped, never double-billed.
	if got, _ := price.IncurredCost(model.Usage{CacheCreationTokens: 100, CacheCreation1hTokens: 500}); got != 600 {
		t.Errorf("IncurredCost(clamped) = %d, want 600", got)
	}
}

func TestRecordUnpricedCountsTokensNotCost(t *testing.T) {
	tr := NewTrackerWithUSDCap(0, 0, 0)
	tr.RecordUnpriced("s1", model.Usage{PromptTokens: 100, CompletionTokens: 7, CacheReadTokens: 50, CacheCreationTokens: 5})
	want := SessionCost{InputTokens: 100, OutputTokens: 7, CacheReadTokens: 50, CacheCreationTokens: 5, UnpricedCalls: 1}
	if got := tr.Cost("s1"); got != want {
		t.Fatalf("Cost() = %+v, want %+v", got, want)
	}
}

func TestReservationCapAndReconciliation(t *testing.T) {
	tr := NewTrackerWithUSDCap(0, 500, 100)

	id, err := tr.Reserve("s1", "claude-sonnet-4-6", 10, 4) // 30 + 60 = 90
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if got := tr.Cost("s1"); got != (SessionCost{ReservedMicrodollars: 90}) {
		t.Fatalf("Cost() after reservation = %+v, want 90 reserved", got)
	}
	if _, err := tr.Reserve("s1", "claude-haiku-4-5", 11, 0); !errors.Is(err, ErrUSDBudgetExceeded) {
		t.Fatalf("over-cap Reserve() error = %v, want ErrUSDBudgetExceeded", err)
	}

	if err := tr.Reconcile(id, 5, 1); err != nil { // 15 + 15 = 30
		t.Fatalf("Reconcile() error = %v", err)
	}
	want := SessionCost{InputTokens: 5, OutputTokens: 1, SpentMicrodollars: 30}
	if got := tr.Cost("s1"); got != want {
		t.Fatalf("Cost() after reconciliation = %+v, want %+v", got, want)
	}
	if _, err := tr.Reserve("s1", "claude-haiku-4-5", 70, 0); err != nil {
		t.Fatalf("Reserve() after unused reservation released error = %v", err)
	}
}

func TestCostSessionIsolationAndUnknowns(t *testing.T) {
	tr := NewTrackerWithUSDCap(0, 500, 50)
	id, err := tr.Reserve("s1", "claude-haiku-4-5", 10, 2)
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if got := tr.Cost("s2"); got != (SessionCost{}) {
		t.Fatalf("Cost(s2) = %+v, want zero", got)
	}
	if _, err := tr.Reserve("s2", "unknown", 1, 1); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("Reserve(unknown) error = %v, want ErrUnknownModel", err)
	}
	if err := tr.Reconcile(id, 10, 2); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if err := tr.Reconcile(id, 10, 2); !errors.Is(err, ErrUnknownReservation) {
		t.Fatalf("second Reconcile() error = %v, want ErrUnknownReservation", err)
	}
}

func TestTrackerHasUSDCap(t *testing.T) {
	if NewTrackerWithUSDCap(0, 0, 0).HasUSDCap() {
		t.Fatal("unlimited tracker reports a USD cap")
	}
	if !NewTrackerWithUSDCap(0, 0, 1).HasUSDCap() {
		t.Fatal("capped tracker does not report its USD cap")
	}
}

func TestConcurrentCostAccounting(t *testing.T) {
	const calls = 100
	tr := NewTrackerWithUSDCap(0, 500, 100_000)
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := tr.Reserve("s1", "claude-haiku-4-5", 10, 10)
			if err != nil {
				t.Errorf("Reserve() error = %v", err)
				return
			}
			if err := tr.Reconcile(id, 4, 2); err != nil {
				t.Errorf("Reconcile() error = %v", err)
			}
		}()
	}
	wg.Wait()

	want := SessionCost{
		InputTokens:       calls * 4,
		OutputTokens:      calls * 2,
		SpentMicrodollars: calls * (4 + 2*5),
	}
	if got := tr.Cost("s1"); got != want {
		t.Fatalf("Cost() = %+v, want %+v", got, want)
	}
}

// seedUsage feeds tokens into tr for the session carried by ctx via After, as
// if a model call had just completed.
func seedUsage(t *testing.T, tr *Tracker, ctx context.Context, tokens int) {
	t.Helper()
	evt := &hooks.Event{
		Type:   hooks.EventModelCallAfter,
		Output: &model.ChatResponse{Usage: model.Usage{PromptTokens: tokens}},
	}
	if err := tr.After(ctx, evt); err != nil {
		t.Fatalf("After returned unexpected error: %v", err)
	}
}

func TestLevelAndCompressionThreshold(t *testing.T) {
	tests := []struct {
		name          string
		used          int
		wantLevel     Level
		wantThreshold int
	}{
		{"0%", 0, LevelNormal, 500},
		{"40%", 400, LevelNormal, 500},
		{"60%", 600, LevelIncreased, 250},
		{"80%", 800, LevelAggressive, 125},
		{"95%", 950, LevelWarn, 62},
		{"110%", 1100, LevelStop, 62},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := NewTracker(1000, 500)
			ctx := storage.WithSession(context.Background(), "s1")
			seedUsage(t, tr, ctx, tt.used)

			if got := tr.Level("s1"); got != tt.wantLevel {
				t.Errorf("Level() = %q, want %q", got, tt.wantLevel)
			}
			if got := tr.CompressionThreshold("s1"); got != tt.wantThreshold {
				t.Errorf("CompressionThreshold() = %d, want %d", got, tt.wantThreshold)
			}
		})
	}
}

func TestCompressionThresholdFloorsAtFiftyForTinyBase(t *testing.T) {
	tr := NewTracker(1000, 10) // baseThreshold/8 == 1, must floor to 50
	ctx := storage.WithSession(context.Background(), "s1")
	seedUsage(t, tr, ctx, 950) // LevelWarn
	if got := tr.CompressionThreshold("s1"); got != 50 {
		t.Errorf("CompressionThreshold() = %d, want 50", got)
	}
}

func TestBeforeBlocksAtBudget(t *testing.T) {
	tr := NewTracker(1000, 500)
	ctx := storage.WithSession(context.Background(), "s1")
	before := &hooks.Event{Type: hooks.EventModelCallBefore, Name: "s1"}

	// Comfortably under budget: Before should not error.
	if err := tr.Before(ctx, before); err != nil {
		t.Fatalf("Before() under budget returned error: %v", err)
	}

	// Push the session at/over budget.
	seedUsage(t, tr, ctx, 1000)

	if err := tr.Before(ctx, before); err == nil {
		t.Fatalf("Before() expected error once budget exceeded, got nil")
	}
}

func TestBeforeAndAfterIgnoreOtherEventTypes(t *testing.T) {
	tr := NewTracker(100, 50)
	ctx := storage.WithSession(context.Background(), "s1")
	seedUsage(t, tr, ctx, 100) // s1 now at budget

	// Before called with a mismatched event type must be a no-op (never block).
	if err := tr.Before(ctx, &hooks.Event{Type: hooks.EventModelCallAfter}); err != nil {
		t.Fatalf("Before() with mismatched event type returned error: %v", err)
	}

	// After called with a mismatched event type, or a failed call, must not
	// count usage.
	if err := tr.After(ctx, &hooks.Event{Type: hooks.EventModelCallBefore}); err != nil {
		t.Fatalf("After() with mismatched event type returned error: %v", err)
	}
	if err := tr.After(ctx, &hooks.Event{
		Type:   hooks.EventModelCallAfter,
		Error:  errNonNil,
		Output: &model.ChatResponse{Usage: model.Usage{PromptTokens: 5}},
	}); err != nil {
		t.Fatalf("After() on failed call returned error: %v", err)
	}
	if got := tr.Used("s1"); got != 100 {
		t.Errorf("Used() = %d after no-op calls, want 100 (unchanged)", got)
	}
}

var errNonNil = context.DeadlineExceeded

func TestSessionIsolation(t *testing.T) {
	tr := NewTracker(1000, 500)
	ctxS1 := storage.WithSession(context.Background(), "s1")
	seedUsage(t, tr, ctxS1, 600)

	if got := tr.Used("s1"); got != 600 {
		t.Errorf("Used(s1) = %d, want 600", got)
	}
	if got := tr.Used("s2"); got != 0 {
		t.Errorf("Used(s2) = %d, want 0 (unaffected by s1 usage)", got)
	}
	if got := tr.Level("s1"); got != LevelIncreased {
		t.Errorf("Level(s1) = %q, want %q", got, LevelIncreased)
	}
	if got := tr.Level("s2"); got != LevelNormal {
		t.Errorf("Level(s2) = %q, want %q", got, LevelNormal)
	}
}

func TestAfterCountsBilledTokensNotCachedPromptWindow(t *testing.T) {
	tr := NewTracker(1_500_000, 500)
	ctx := storage.WithSession(context.Background(), "s1")
	evt := &hooks.Event{
		Type: hooks.EventModelCallAfter,
		Output: &model.ChatResponse{Usage: model.Usage{
			PromptTokens:        400,
			CompletionTokens:    50,
			CacheReadTokens:     10000,
			CacheCreationTokens: 80,
		}},
	}
	if err := tr.After(ctx, evt); err != nil {
		t.Fatalf("After() error = %v", err)
	}
	// Uncached 400 + cache write 80 + completion 50. Cache hits must not
	// count or a long cached tool loop exhausts the session cap.
	if got := tr.Used("s1"); got != 530 {
		t.Errorf("Used() = %d, want 530", got)
	}

	openaiStyle := &hooks.Event{
		Type: hooks.EventModelCallAfter,
		Output: &model.ChatResponse{Usage: model.Usage{
			PromptTokens:      10400,
			CompletionTokens:  20,
			CacheReadTokens:   10000,
			CacheReadInPrompt: true,
		}},
	}
	if err := tr.After(ctx, openaiStyle); err != nil {
		t.Fatalf("After() openai-style error = %v", err)
	}
	if got := tr.Used("s1"); got != 530+420 {
		t.Errorf("Used() after openai-style call = %d, want 950", got)
	}
}

func TestFallsBackToEventNameWithoutSession(t *testing.T) {
	tr := NewTracker(1000, 500)
	ctx := context.Background() // no session set
	evt := &hooks.Event{
		Type:   hooks.EventModelCallAfter,
		Name:   "gpt-4",
		Output: &model.ChatResponse{Usage: model.Usage{PromptTokens: 42, CompletionTokens: 8}},
	}
	if err := tr.After(ctx, evt); err != nil {
		t.Fatalf("After() returned error: %v", err)
	}
	if got := tr.Used("gpt-4"); got != 50 {
		t.Errorf("Used(gpt-4) = %d, want 50", got)
	}
}

func TestUnlimitedTracker(t *testing.T) {
	tr := NewTracker(0, 500)
	ctx := storage.WithSession(context.Background(), "s1")

	if got := tr.Remaining("s1"); got != -1 {
		t.Errorf("Remaining() = %d, want -1 (unlimited sentinel)", got)
	}

	seedUsage(t, tr, ctx, 999999)

	if got := tr.Level("s1"); got != LevelNormal {
		t.Errorf("Level() = %q, want %q for unlimited tracker", got, LevelNormal)
	}
	if got := tr.Ratio("s1"); got != 0 {
		t.Errorf("Ratio() = %v, want 0 for unlimited tracker", got)
	}
	if err := tr.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore}); err != nil {
		t.Errorf("Before() returned error for unlimited tracker: %v", err)
	}
}

func TestStatusLine(t *testing.T) {
	tr := NewTracker(1000, 500)
	ctx := storage.WithSession(context.Background(), "s1")
	seedUsage(t, tr, ctx, 500)

	line := tr.StatusLine("s1")
	for _, want := range []string{"500", "1000", "50%", "increased"} {
		if !strings.Contains(line, want) {
			t.Errorf("StatusLine() = %q, want substring %q", line, want)
		}
	}

	unlimited := NewTracker(0, 500)
	ctxU := storage.WithSession(context.Background(), "s1")
	seedUsage(t, unlimited, ctxU, 12345)

	lineU := unlimited.StatusLine("s1")
	for _, want := range []string{"unlimited", "12345"} {
		if !strings.Contains(lineU, want) {
			t.Errorf("StatusLine() = %q, want substring %q", lineU, want)
		}
	}
}

func TestReconcileUsageAppliesCacheReadDiscount(t *testing.T) {
	tr := NewTrackerWithUSDCap(0, 500, 0)
	id, err := tr.Reserve("s1", "claude-sonnet-4-6", 400, 2)
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	err = tr.ReconcileUsage(id, model.Usage{
		PromptTokens:        400,
		CompletionTokens:    2,
		CacheReadTokens:     10000,
		CacheCreationTokens: 80,
	})
	if err != nil {
		t.Fatalf("ReconcileUsage() error = %v", err)
	}
	// uncached 400*3 + write 80*3*5/4 + read 10000*3/10 + out 2*15
	// = 1200 + 300 + 3000 + 30 = 4530
	want := SessionCost{
		InputTokens:         400,
		OutputTokens:        2,
		CacheReadTokens:     10000,
		CacheCreationTokens: 80,
		SpentMicrodollars:   4530,
	}
	if got := tr.Cost("s1"); got != want {
		t.Fatalf("Cost() = %+v, want %+v", got, want)
	}
}
