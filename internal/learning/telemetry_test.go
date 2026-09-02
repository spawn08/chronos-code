package learning

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/storage"
)

func TestTelemetryMapsModelAndToolEvents(t *testing.T) {
	ctx := storage.WithSession(context.Background(), "session-1")
	store := openTestSQLStore(t)
	recorder := NewTelemetryRecorder(store, "/repo", "coder")
	events := []*hooks.Event{
		{Type: hooks.EventModelCallBefore, Name: "model-a", Input: map[string]any{"prompt": "fix"}, Metadata: map[string]any{"stream": true}},
		{Type: hooks.EventToolCallBefore, Name: "file_read", Input: map[string]any{"path": "a.go"}},
		{Type: hooks.EventToolCallAfter, Name: "file_read", Input: map[string]any{"path": "a.go"}, Output: "package a"},
		{Type: hooks.EventModelCallAfter, Name: "model-a", Output: map[string]any{"content": "fixed"}, Error: errors.New("provider error")},
	}

	if err := recorder.Before(ctx, events[0]); err != nil {
		t.Fatalf("Before(model) error = %v", err)
	}
	if err := recorder.Before(ctx, events[1]); err != nil {
		t.Fatalf("Before(tool) error = %v", err)
	}
	if err := recorder.After(ctx, events[2]); err != nil {
		t.Fatalf("After(tool) error = %v", err)
	}
	if err := recorder.After(ctx, events[3]); err != nil {
		t.Fatalf("After(model) error = %v", err)
	}

	var repoPath, model string
	if err := store.db.QueryRow(`SELECT repo_path, model FROM sessions WHERE id = 'session-1'`).Scan(&repoPath, &model); err != nil {
		t.Fatalf("query session: %v", err)
	}
	if repoPath != "/repo" || model != "model-a" {
		t.Errorf("session metadata = %q, %q, want /repo, model-a", repoPath, model)
	}

	rows, err := store.db.Query(`SELECT role, content, ts FROM turns ORDER BY role`)
	if err != nil {
		t.Fatalf("query turns: %v", err)
	}
	defer rows.Close()
	wantContent := map[string]string{
		string(hooks.EventModelCallAfter):  `{"name":"model-a","output":{"content":"fixed"},"error":"provider error"}`,
		string(hooks.EventModelCallBefore): `{"name":"model-a","input":{"prompt":"fix"},"metadata":{"stream":true}}`,
		string(hooks.EventToolCallAfter):   `{"name":"file_read","input":{"path":"a.go"},"output":"package a"}`,
		string(hooks.EventToolCallBefore):  `{"name":"file_read","input":{"path":"a.go"}}`,
	}
	gotContent := make(map[string]string)
	for rows.Next() {
		var role, content, ts string
		if err := rows.Scan(&role, &content, &ts); err != nil {
			t.Fatalf("scan turn: %v", err)
		}
		if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
			t.Errorf("turn timestamp %q is invalid: %v", ts, err)
		}
		gotContent[role] = content
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate turns: %v", err)
	}
	if !reflect.DeepEqual(gotContent, wantContent) {
		t.Errorf("turn mapping = %#v, want %#v", gotContent, wantContent)
	}

	callRows, err := store.db.Query(`
		SELECT t.role, tc.name, tc.input, tc.output
		FROM tool_calls tc JOIN turns t ON t.id = tc.turn_id
		ORDER BY t.role`)
	if err != nil {
		t.Fatalf("query tool calls: %v", err)
	}
	defer callRows.Close()
	wantCalls := [][4]string{
		{string(hooks.EventToolCallAfter), "file_read", `{"path":"a.go"}`, `"package a"`},
		{string(hooks.EventToolCallBefore), "file_read", `{"path":"a.go"}`, ""},
	}
	var gotCalls [][4]string
	for callRows.Next() {
		var call [4]string
		if err := callRows.Scan(&call[0], &call[1], &call[2], &call[3]); err != nil {
			t.Fatalf("scan tool call: %v", err)
		}
		gotCalls = append(gotCalls, call)
	}
	if !reflect.DeepEqual(gotCalls, wantCalls) {
		t.Errorf("tool mapping = %#v, want %#v", gotCalls, wantCalls)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.Outcomes != 0 {
		t.Errorf("Stats().Outcomes = %d, want 0", stats.Outcomes)
	}
}

func TestTelemetryUsesContextSessionWithAgentFallback(t *testing.T) {
	store := openTestSQLStore(t)
	recorder := NewTelemetryRecorder(store, "/repo", "coder")
	evt := &hooks.Event{Type: hooks.EventModelCallBefore, Name: "model-a"}

	if err := recorder.Before(context.Background(), evt); err != nil {
		t.Fatalf("Before() fallback error = %v", err)
	}
	if err := recorder.Before(storage.WithSession(context.Background(), "explicit"), evt); err != nil {
		t.Fatalf("Before() explicit session error = %v", err)
	}

	rows, err := store.db.Query(`SELECT id FROM sessions ORDER BY id`)
	if err != nil {
		t.Fatalf("query sessions: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan session: %v", err)
		}
		got = append(got, id)
	}
	if want := []string{"coder", "explicit"}; !reflect.DeepEqual(got, want) {
		t.Errorf("session IDs = %v, want %v", got, want)
	}
}

func TestTelemetryConcurrentIDsAreUnique(t *testing.T) {
	store := openTestSQLStore(t)
	recorder := NewTelemetryRecorder(store, "/repo", "coder")
	ctx := storage.WithSession(context.Background(), "concurrent")
	const writers = 40

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- recorder.After(ctx, &hooks.Event{
				Type:  hooks.EventToolCallAfter,
				Name:  "tool",
				Input: map[string]any{"writer": i},
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent record error = %v", err)
		}
	}

	var turns, distinctTurns, calls, distinctCalls int
	if err := store.db.QueryRow(`SELECT count(*), count(DISTINCT id) FROM turns`).Scan(&turns, &distinctTurns); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if err := store.db.QueryRow(`SELECT count(*), count(DISTINCT id) FROM tool_calls`).Scan(&calls, &distinctCalls); err != nil {
		t.Fatalf("count calls: %v", err)
	}
	if turns != writers || distinctTurns != writers || calls != writers || distinctCalls != writers {
		t.Errorf("counts = turns %d/%d, calls %d/%d, want all %d", turns, distinctTurns, calls, distinctCalls, writers)
	}
}

func TestTelemetryIgnoresUnsupportedEventsAndReturnsStorageErrors(t *testing.T) {
	store := openTestSQLStore(t)
	recorder := NewTelemetryRecorder(store, "/repo", "coder")
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if err := recorder.Before(context.Background(), &hooks.Event{Type: hooks.EventNodeBefore}); err != nil {
		t.Errorf("Before(unsupported) error = %v", err)
	}
	if err := recorder.After(context.Background(), &hooks.Event{Type: hooks.EventSessionEnd}); err != nil {
		t.Errorf("After(unsupported) error = %v", err)
	}
	if err := recorder.Before(context.Background(), nil); err != nil {
		t.Errorf("Before(nil) error = %v", err)
	}
	if err := recorder.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Name: "model"}); err == nil {
		t.Fatal("Before(supported) storage error = nil")
	}
}
