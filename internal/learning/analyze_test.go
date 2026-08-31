package learning

import (
	"context"
	"testing"

	chronostrace "github.com/spawn08/chronos/os/trace"
	"github.com/spawn08/chronos/storage/adapters/sqlite"
)

func newTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite.New(:memory:): %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestAnalyze_AggregatesToolCallsAndSequences(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	collector := chronostrace.NewCollector(store)

	// Session 1: file_read -> file_grep -> file_read, one erroring model call.
	span, err := collector.StartSpan(ctx, "sess_1", "model:claude", "model_call")
	if err != nil {
		t.Fatalf("StartSpan: %v", err)
	}
	if err := collector.EndSpan(ctx, span, nil, "rate limited"); err != nil {
		t.Fatalf("EndSpan: %v", err)
	}
	for _, tool := range []string{"tool:file_read", "tool:file_grep", "tool:file_read"} {
		span, err := collector.StartSpan(ctx, "sess_1", tool, "tool_call")
		if err != nil {
			t.Fatalf("StartSpan: %v", err)
		}
		if err := collector.EndSpan(ctx, span, "ok", ""); err != nil {
			t.Fatalf("EndSpan: %v", err)
		}
	}

	// Session 2: file_read -> file_grep again, no errors.
	for _, tool := range []string{"tool:file_read", "tool:file_grep"} {
		span, err := collector.StartSpan(ctx, "sess_2", tool, "tool_call")
		if err != nil {
			t.Fatalf("StartSpan: %v", err)
		}
		if err := collector.EndSpan(ctx, span, "ok", ""); err != nil {
			t.Fatalf("EndSpan: %v", err)
		}
	}

	report, err := Analyze(ctx, store, "coder", []string{"sess_1", "sess_2"})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}

	if report.Empty() {
		t.Fatal("report.Empty() = true, want false")
	}
	if report.TotalSpans != 6 {
		t.Errorf("TotalSpans = %d, want 6", report.TotalSpans)
	}
	if report.ModelCalls != 1 {
		t.Errorf("ModelCalls = %d, want 1", report.ModelCalls)
	}
	if report.ToolCalls != 5 {
		t.Errorf("ToolCalls = %d, want 5", report.ToolCalls)
	}
	if report.Errors != 1 {
		t.Errorf("Errors = %d, want 1", report.Errors)
	}
	if report.ToolCounts["file_read"] != 3 {
		t.Errorf("ToolCounts[file_read] = %d, want 3", report.ToolCounts["file_read"])
	}
	if report.ToolCounts["file_grep"] != 2 {
		t.Errorf("ToolCounts[file_grep] = %d, want 2", report.ToolCounts["file_grep"])
	}
	if report.ToolSequences["file_read>file_grep"] != 2 {
		t.Errorf("ToolSequences[file_read>file_grep] = %d, want 2", report.ToolSequences["file_read>file_grep"])
	}
	// Bigram must not cross a session boundary: session 1 ends on file_read,
	// session 2 starts fresh with file_read, so "file_read>file_read" (from
	// session 1's grep->read) is the only same-session read-after-read case,
	// and there must be no leaked cross-session bigram.
	if report.ToolSequences["file_grep>file_read"] != 1 {
		t.Errorf("ToolSequences[file_grep>file_read] = %d, want 1", report.ToolSequences["file_grep>file_read"])
	}

	summary := report.Summary()
	if summary == "" {
		t.Error("Summary() returned empty string")
	}
}

func TestAnalyze_NoTraces(t *testing.T) {
	store := newTestStore(t)
	report, err := Analyze(context.Background(), store, "coder", []string{"sess_empty"})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if !report.Empty() {
		t.Error("report.Empty() = false, want true for a session with no traces")
	}
}
