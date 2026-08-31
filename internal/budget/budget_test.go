package budget

import (
	"context"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/storage"
)

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
