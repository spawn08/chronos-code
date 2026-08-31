package attention

import (
	"context"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/storage"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		tool string
		args map[string]any
		want Category
	}{
		{"file_write", nil, CatEditTarget},
		{"update_plan", nil, CatPlan},
		{"create_plan", nil, CatPlan},
		{"shell", map[string]any{"command": "go test ./..."}, CatTestOutput},
		{"shell", map[string]any{"command": "ls -la"}, CatOther},
		{"shell", map[string]any{"command": "make test"}, CatTestOutput},
		{"graph_query", nil, CatGraph},
		{"find_callers", nil, CatGraph},
		{"find_implementations", nil, CatGraph},
		{"resolve_symbol", nil, CatGraph},
		{"impact_analysis", nil, CatGraph},
		{"test_map", nil, CatGraph},
		{"co_change", nil, CatGraph},
		{"file_read", nil, CatRead},
		{"file_list", nil, CatRead},
		{"file_glob", nil, CatRead},
		{"file_grep", nil, CatRead},
		{"workspace_info", nil, CatRead},
		{"semantic_search", nil, CatRead},
		{"unknown_tool", nil, CatOther},
	}
	for _, tt := range tests {
		got := Classify(tt.tool, tt.args)
		if got != tt.want {
			t.Errorf("Classify(%q, %v) = %q, want %q", tt.tool, tt.args, got, tt.want)
		}
	}
}

func TestWeight(t *testing.T) {
	if w := Weight(CatEditTarget); w != 1.0 {
		t.Errorf("edit_target weight=%f, want 1.0", w)
	}
	if w := Weight(CatRead); w != 0.3 {
		t.Errorf("read weight=%f, want 0.3", w)
	}
	if w := Weight(Category("nonexistent")); w != 0.1 {
		t.Errorf("unknown weight=%f, want 0.1", w)
	}
}

func TestAdjustThreshold(t *testing.T) {
	base := 500

	high := AdjustThreshold(base, 1.0)
	if high != 1000 {
		t.Errorf("high attention: got %d, want 1000", high)
	}

	low := AdjustThreshold(base, 0.1)
	if low < 50 || low > 250 {
		t.Errorf("low attention: got %d, want [50,250]", low)
	}

	mid := AdjustThreshold(base, 0.5)
	if mid <= low || mid >= high {
		t.Errorf("mid attention: got %d, want between %d and %d", mid, low, high)
	}

	floor := AdjustThreshold(10, 0.0)
	if floor < 50 {
		t.Errorf("floor: got %d, want >= 50", floor)
	}
}

func TestBudgeter_AfterRecordsCalls(t *testing.T) {
	b := NewBudgeter(10)
	ctx := storage.WithSession(context.Background(), "sess-1")

	b.After(ctx, &hooks.Event{
		Type:  hooks.EventToolCallAfter,
		Name:  "file_write",
		Input: map[string]any{},
	})
	b.After(ctx, &hooks.Event{
		Type:  hooks.EventToolCallAfter,
		Name:  "file_read",
		Input: map[string]any{},
	})

	dist := b.CategoryDistribution("sess-1")
	if dist[CatEditTarget] != 1 {
		t.Errorf("edit_target count=%d, want 1", dist[CatEditTarget])
	}
	if dist[CatRead] != 1 {
		t.Errorf("read count=%d, want 1", dist[CatRead])
	}
}

func TestBudgeter_CurrentWeight(t *testing.T) {
	b := NewBudgeter(10)
	ctx := storage.WithSession(context.Background(), "sess-2")

	w := b.CurrentWeight("sess-2")
	if w != 0.5 {
		t.Errorf("empty session weight=%f, want 0.5", w)
	}

	b.After(ctx, &hooks.Event{
		Type: hooks.EventToolCallAfter,
		Name: "file_write",
	})
	w = b.CurrentWeight("sess-2")
	if w != 1.0 {
		t.Errorf("after file_write weight=%f, want 1.0", w)
	}

	b.After(ctx, &hooks.Event{
		Type: hooks.EventToolCallAfter,
		Name: "file_read",
	})
	w = b.CurrentWeight("sess-2")
	if w != 0.3 {
		t.Errorf("after file_read weight=%f, want 0.3", w)
	}
}

func TestBudgeter_IgnoresNonToolEvents(t *testing.T) {
	b := NewBudgeter(10)
	ctx := storage.WithSession(context.Background(), "sess-3")

	b.After(ctx, &hooks.Event{
		Type: hooks.EventModelCallAfter,
		Name: "claude-sonnet",
	})

	dist := b.CategoryDistribution("sess-3")
	if len(dist) != 0 {
		t.Errorf("expected no records for model call event, got %v", dist)
	}
}

func TestBudgeter_MaxRecords(t *testing.T) {
	b := NewBudgeter(3)
	ctx := storage.WithSession(context.Background(), "sess-4")

	for i := 0; i < 5; i++ {
		b.After(ctx, &hooks.Event{
			Type: hooks.EventToolCallAfter,
			Name: "file_read",
		})
	}

	b.mu.Lock()
	count := len(b.calls["sess-4"])
	b.mu.Unlock()
	if count != 3 {
		t.Errorf("record count=%d, want 3 (max)", count)
	}
}

func TestBudgeter_HighAttentionSummary(t *testing.T) {
	b := NewBudgeter(10)
	ctx := storage.WithSession(context.Background(), "sess-5")

	if s := b.HighAttentionSummary("sess-5"); s != "" {
		t.Errorf("empty session summary=%q, want empty", s)
	}

	b.After(ctx, &hooks.Event{Type: hooks.EventToolCallAfter, Name: "file_read"})
	if s := b.HighAttentionSummary("sess-5"); s != "exploring" {
		t.Errorf("read-only summary=%q, want exploring", s)
	}

	b.After(ctx, &hooks.Event{Type: hooks.EventToolCallAfter, Name: "file_write"})
	if s := b.HighAttentionSummary("sess-5"); s != "editing" {
		t.Errorf("after write summary=%q, want editing", s)
	}

	b.After(ctx, &hooks.Event{
		Type:  hooks.EventToolCallAfter,
		Name:  "shell",
		Input: map[string]any{"command": "go test ./..."},
	})
	if s := b.HighAttentionSummary("sess-5"); s != "editing+testing" {
		t.Errorf("after test summary=%q, want editing+testing", s)
	}
}
