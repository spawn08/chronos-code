package session

import (
	"context"
	"errors"
	"fmt"
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

type summaryRecallSpy struct {
	storage.Storage
	t                     *testing.T
	sessions              []*storage.Session
	listCalls, eventCalls int
	readErr               error
}

func (s *summaryRecallSpy) ListSessions(ctx context.Context, agentID string, limit, offset int) ([]*storage.Session, error) {
	s.listCalls++
	if storage.TenantFromContext(ctx) != "recall-tenant" || agentID != "coder" || limit != 100 || offset != 0 {
		s.t.Fatalf("ListSessions lost scope or bounds: tenant=%q agent=%q limit=%d offset=%d", storage.TenantFromContext(ctx), agentID, limit, offset)
	}
	// Deliberately ignore the requested limit to exercise the caller's hard cap.
	return s.sessions, nil
}

func (s *summaryRecallSpy) ListEvents(ctx context.Context, id string, afterSeq int64) ([]*storage.Event, error) {
	s.eventCalls++
	if storage.TenantFromContext(ctx) != "recall-tenant" || afterSeq != 0 {
		s.t.Fatal("legacy read lost tenant or replay compatibility")
	}
	return []*storage.Event{event(7, "chat_summary", map[string]any{"summary": "legacy " + id})}, s.readErr
}

type projectedSummarySpy struct {
	*summaryRecallSpy
	reads int
}

func (s *projectedSummarySpy) ReadSessionSummary(ctx context.Context, id string, maxBytes int) (string, string, int64, bool, error) {
	s.reads++
	if storage.TenantFromContext(ctx) != "recall-tenant" || maxBytes != 2000 {
		s.t.Fatalf("projected read lost tenant or budget: %q, %d", storage.TenantFromContext(ctx), maxBytes)
	}
	return "projected " + id, "chat_summary", 9, true, s.readErr
}

func TestRecallSummariesCandidateCapAndOptionalReader(t *testing.T) {
	for _, projected := range []bool{false, true} {
		t.Run(fmt.Sprintf("projected=%t", projected), func(t *testing.T) {
			spy := &summaryRecallSpy{t: t}
			for i := 0; i < 150; i++ {
				spy.sessions = append(spy.sessions, &storage.Session{ID: fmt.Sprintf("session-%03d", i), AgentID: "coder", UpdatedAt: time.Now()})
			}
			spy.sessions[0].ID = "active"
			spy.sessions[1].AgentID = "other-agent"
			spy.sessions[2] = nil
			capable := &projectedSummarySpy{summaryRecallSpy: spy}
			var store storage.Storage = spy
			if projected {
				store = capable
			}
			ctx := storage.WithTenant(context.Background(), "recall-tenant")
			mgr := NewManager(store, "")
			got, err := mgr.RecallSummaries(ctx, "coder", "active", "099", 3, 2000)
			if err != nil || len(got) != 3 || got[0].SessionID != "session-099" {
				t.Fatalf("bounded ranking = %#v, %v", got, err)
			}
			if spy.listCalls != 1 {
				t.Fatalf("ListSessions calls = %d, want 1", spy.listCalls)
			}
			if projected {
				if spy.eventCalls != 0 || capable.reads != 97 || !got[0].Truncated || got[0].SourceSeq != 9 {
					t.Fatalf("projected reads=%d full reads=%d summary=%#v", capable.reads, spy.eventCalls, got[0])
				}
			} else if spy.eventCalls != 97 || got[0].SourceSeq != 7 {
				t.Fatalf("legacy reads=%d summary=%#v", spy.eventCalls, got[0])
			}
			spy.readErr = errors.New("read failed")
			if _, err := mgr.RecallSummaries(ctx, "coder", "active", "", 1, 20); !errors.Is(err, spy.readErr) {
				t.Fatalf("read error = %v", err)
			}
			calls := spy.listCalls
			for _, limits := range [][2]int{{0, 2000}, {3, 0}, {-1, 2000}, {3, -1}} {
				if got, err := mgr.RecallSummaries(ctx, "coder", "", "", limits[0], limits[1]); err != nil || len(got) != 0 || spy.listCalls != calls {
					t.Fatalf("disabled recall performed I/O: %#v, %v", got, err)
				}
			}
		})
	}
}

func TestRecallSummariesLegacyAdapter(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	createSummarySession(t, store, ctx, "legacy", "coder", time.Now(),
		event(1, "chat_message", map[string]any{"role": "user", "content": "question"}),
		event(2, "chat_message", map[string]any{"role": "tool", "content": "excluded"}),
		event(3, "chat_message", map[string]any{"role": "assistant", "content": "answer"}))
	// Embedding just Storage hides the SQLite-only optional capability.
	legacy := struct{ storage.Storage }{store}
	got, err := NewManager(legacy, "").RecallSummaries(ctx, "coder", "", "answer", 3, 2000)
	if err != nil || len(got) != 1 || got[0].Text != "user: question\nassistant: answer" || got[0].SourceSeq != 3 {
		t.Fatalf("legacy fallback = %#v, %v", got, err)
	}
}
