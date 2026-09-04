package session

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spawn08/chronos/storage"
)

func TestRecallSummariesIsolationPreferenceAndMalformedOmission(t *testing.T) {
	store := newTestStore(t)
	mgr := NewManager(store, "")
	ctxA := storage.WithTenant(context.Background(), "tenant-a")
	ctxB := storage.WithTenant(context.Background(), "tenant-b")
	now := time.Now()

	createSummarySession(t, store, ctxA, "active", "coder", now.Add(4*time.Minute),
		event(1, "chat_summary", map[string]any{"summary": "active secret"}))
	createSummarySession(t, store, ctxA, "other-agent", "reviewer", now.Add(3*time.Minute),
		event(1, "chat_summary", map[string]any{"summary": "reviewer secret"}))
	createSummarySession(t, store, ctxB, "other-tenant", "coder", now.Add(2*time.Minute),
		event(1, "chat_summary", map[string]any{"summary": "tenant secret"}))
	createSummarySession(t, store, ctxA, "prior", "coder", now,
		event(1, "chat_message", map[string]any{"role": "user", "content": "obsolete fallback"}),
		event(2, "chat_summary", map[string]any{"summary": "valid older summary"}),
		event(3, "chat_summary", map[string]any{"summary": "  "}),
		event(4, "chat_summary", map[string]any{"summary": "latest valid summary"}),
		event(5, "chat_summary", []string{"malformed"}))
	createSummarySession(t, store, ctxA, "fallback", "coder", now.Add(time.Minute),
		event(1, "chat_message", map[string]any{"role": "tool", "content": "tool secret"}),
		event(2, "chat_message", map[string]any{"role": "user", "content": "fallback user"}),
		event(3, "chat_message", map[string]any{"role": "assistant", "content": "fallback answer"}),
		event(4, "chat_message", map[string]any{"role": "assistant", "content": ""}),
		event(5, "chat_message", "malformed"))

	got, err := mgr.RecallSummaries(ctxA, "coder", "active", "summary", 10, 1000)
	if err != nil {
		t.Fatalf("RecallSummaries() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("RecallSummaries() = %#v, want two prior same-tenant coder sessions", got)
	}
	if got[0].SessionID != "prior" || got[0].Text != "latest valid summary" || got[0].Source != "chat_summary" || got[0].SourceSeq != 4 {
		t.Fatalf("preferred summary = %#v", got[0])
	}
	if got[1].SessionID != "fallback" || got[1].Text != "user: fallback user\nassistant: fallback answer" || got[1].Source != "chat_message" {
		t.Fatalf("fallback summary = %#v", got[1])
	}
	joined := got[0].Text + got[1].Text
	for _, excluded := range []string{"active secret", "reviewer secret", "tenant secret", "tool secret", "obsolete fallback"} {
		if strings.Contains(joined, excluded) {
			t.Fatalf("selected summaries leaked %q: %#v", excluded, got)
		}
	}
}

func TestRecallSummariesDeterministicRankingAndBounds(t *testing.T) {
	store := newTestStore(t)
	mgr := NewManager(store, "")
	ctx := context.Background()
	now := time.Now()
	createSummarySession(t, store, ctx, "z-new", "coder", now.Add(time.Minute), event(1, "chat_summary", map[string]any{"summary": "unrelated newest"}))
	createSummarySession(t, store, ctx, "b-match", "coder", now, event(1, "chat_summary", map[string]any{"summary": "alpha beta plus"}))
	createSummarySession(t, store, ctx, "a-match", "coder", now, event(1, "chat_summary", map[string]any{"summary": "alpha beta and more"}))
	createSummarySession(t, store, ctx, "utf8-fallback", "coder", now.Add(-time.Minute), event(1, "chat_message", map[string]any{"role": "user", "content": "ééé"}))

	first, err := mgr.RecallSummaries(ctx, "coder", "", "alpha beta", 2, 20)
	if err != nil {
		t.Fatalf("RecallSummaries() error = %v", err)
	}
	second, err := mgr.RecallSummaries(ctx, "coder", "", "alpha beta", 2, 20)
	if err != nil {
		t.Fatalf("RecallSummaries() second error = %v", err)
	}
	if len(first) != 2 || first[0].SessionID != "a-match" || first[1].SessionID != "b-match" {
		t.Fatalf("ranked summaries = %#v, want stable ID tie-break", first)
	}
	if first[0] != second[0] || first[1] != second[1] {
		t.Fatalf("repeated selection differs: %#v vs %#v", first, second)
	}
	total := 0
	for _, summary := range first {
		total += len(summary.Text)
	}
	if total > 20 || !first[1].Truncated {
		t.Fatalf("total bytes = %d, summaries = %#v; want <= 20 with truncation", total, first)
	}

	utf8Summary, err := mgr.RecallSummaries(ctx, "coder", "", "ééé", 1, 10)
	if err != nil {
		t.Fatalf("RecallSummaries() UTF-8 error = %v", err)
	}
	if len(utf8Summary) != 1 || utf8Summary[0].Source != "chat_message" || !utf8Summary[0].Truncated || len(utf8Summary[0].Text) > 10 || !utf8.ValidString(utf8Summary[0].Text) {
		t.Fatalf("UTF-8 bounded fallback = %#v", utf8Summary)
	}
}

func createSummarySession(t *testing.T, store storage.Storage, ctx context.Context, id, agentID string, updated time.Time, events ...*storage.Event) {
	t.Helper()
	if err := store.CreateSession(ctx, &storage.Session{ID: id, AgentID: agentID, Status: "completed", CreatedAt: updated, UpdatedAt: updated}); err != nil {
		t.Fatalf("CreateSession(%q): %v", id, err)
	}
	for i, storedEvent := range events {
		storedEvent.ID = id + "-event-" + time.Duration(i).String()
		storedEvent.SessionID = id
		if err := store.AppendEvent(ctx, storedEvent); err != nil {
			t.Fatalf("AppendEvent(%q): %v", id, err)
		}
	}
}

func event(seq int64, eventType string, payload any) *storage.Event {
	return &storage.Event{SeqNum: seq, Type: eventType, Payload: payload, CreatedAt: time.Now()}
}
