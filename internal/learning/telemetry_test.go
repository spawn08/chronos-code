package learning

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/storage"
)

func TestTelemetryCorrelatesModelAndToolEvents(t *testing.T) {
	ctx := storage.WithSession(context.Background(), "session-1")
	store := openTestSQLStore(t)
	recorder := NewTelemetryRecorder(store, "/repo", "coder")
	modelEvent := &hooks.Event{
		Type:     hooks.EventModelCallBefore,
		Name:     "model-a",
		Input:    map[string]any{"prompt": "prompt-secret"},
		Metadata: map[string]any{"correlation_id": "model-call-1"},
	}
	toolEvent := &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_read", Input: map[string]any{"path": "source-secret.go"}}

	if err := recorder.Before(ctx, modelEvent); err != nil {
		t.Fatalf("Before(model) error = %v", err)
	}
	if err := recorder.Before(ctx, toolEvent); err != nil {
		t.Fatalf("Before(tool) error = %v", err)
	}
	modelEvent.Type = hooks.EventModelCallAfter
	modelEvent.Output = &model.ChatResponse{Content: "response-secret", Usage: model.Usage{PromptTokens: 3, CompletionTokens: 5}}
	if err := recorder.After(ctx, modelEvent); err != nil {
		t.Fatalf("After(model) error = %v", err)
	}
	toolEvent.Type = hooks.EventToolCallAfter
	toolEvent.Output = "source-secret"
	if err := recorder.After(ctx, toolEvent); err != nil {
		t.Fatalf("After(tool) error = %v", err)
	}
	if err := recorder.After(ctx, toolEvent); err != nil {
		t.Fatalf("After(tool duplicate) error = %v", err)
	}

	var repoPath, sessionModel string
	var turns, inputTokens, outputTokens int
	if err := store.db.QueryRow(`SELECT repo_path, model, turns, input_tokens, output_tokens FROM sessions WHERE id = 'session-1'`).Scan(&repoPath, &sessionModel, &turns, &inputTokens, &outputTokens); err != nil {
		t.Fatalf("query session: %v", err)
	}
	if repoPath != "/repo" || sessionModel != "model-a" {
		t.Errorf("session metadata = %q, %q, want /repo, model-a", repoPath, sessionModel)
	}
	if turns != 2 || inputTokens != 3 || outputTokens != 5 {
		t.Errorf("session totals = turns %d, input %d, output %d, want 2, 3, 5", turns, inputTokens, outputTokens)
	}

	rows, err := store.db.Query(`SELECT role, content, ts FROM turns ORDER BY role`)
	if err != nil {
		t.Fatalf("query turns: %v", err)
	}
	defer rows.Close()
	gotRoles := make(map[string]bool)
	for rows.Next() {
		var role, content, ts string
		if err := rows.Scan(&role, &content, &ts); err != nil {
			t.Fatalf("scan turn: %v", err)
		}
		if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
			t.Errorf("turn timestamp %q is invalid: %v", ts, err)
		}
		for _, fixture := range []string{"prompt-secret", "source-secret", "response-secret"} {
			if contains := strings.Contains(content, fixture); contains {
				t.Errorf("telemetry content contains raw fixture %q: %s", fixture, content)
			}
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(content), &record); err != nil {
			t.Fatalf("decode telemetry content: %v", err)
		}
		if record["state"] != "completed" {
			t.Errorf("telemetry state = %v, want completed", record["state"])
		}
		gotRoles[role] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate turns: %v", err)
	}
	if !reflect.DeepEqual(gotRoles, map[string]bool{"model_call": true, "tool_call": true}) {
		t.Errorf("turn roles = %#v, want one model and one tool call", gotRoles)
	}

	callRows, err := store.db.Query(`
		SELECT t.role, tc.name, tc.input, tc.output, tc.duration_ms
		FROM tool_calls tc JOIN turns t ON t.id = tc.turn_id
		ORDER BY t.role`)
	if err != nil {
		t.Fatalf("query tool calls: %v", err)
	}
	defer callRows.Close()
	var gotCalls int
	for callRows.Next() {
		var role, name, input, output string
		var duration int
		if err := callRows.Scan(&role, &name, &input, &output, &duration); err != nil {
			t.Fatalf("scan tool call: %v", err)
		}
		if role != "tool_call" || name != "file_read" || input == "" || output == "" || duration < 0 {
			t.Errorf("tool call = (%q, %q, %q, %q, %d), want completed privacy-safe call", role, name, input, output, duration)
		}
		if strings.Contains(input, "source-secret") || strings.Contains(output, "source-secret") {
			t.Errorf("tool call contains raw source fixture")
		}
		gotCalls++
	}
	if gotCalls != 1 {
		t.Errorf("tool call records = %d, want 1", gotCalls)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.Turns != 2 || stats.ToolCalls != 1 || stats.Outcomes != 0 {
		t.Errorf("Stats() = %#v, want 2 turns, 1 tool call, and 0 outcomes", stats)
	}
}

func TestTelemetryRetainsIncompleteCalls(t *testing.T) {
	ctx := storage.WithSession(context.Background(), "interrupted")
	store := openTestSQLStore(t)
	recorder := NewTelemetryRecorder(store, "/repo", "coder")
	evt := &hooks.Event{Type: hooks.EventToolCallBefore, Name: "shell", Input: "secret-command"}

	if err := recorder.Before(ctx, evt); err != nil {
		t.Fatalf("Before() error = %v", err)
	}

	var content string
	if err := store.db.QueryRow(`SELECT content FROM turns`).Scan(&content); err != nil {
		t.Fatalf("query incomplete turn: %v", err)
	}
	if !strings.Contains(content, `"state":"pending"`) || strings.Contains(content, "secret-command") {
		t.Errorf("incomplete telemetry content = %s, want diagnosable privacy-safe pending record", content)
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

func TestTelemetryResumesExistingSession(t *testing.T) {
	store := openTestSQLStore(t)
	ctx := storage.WithSession(context.Background(), "resumed-session")
	evt := &hooks.Event{Type: hooks.EventModelCallBefore, Name: "model-a"}

	first := NewTelemetryRecorder(store, "/repo", "coder")
	if err := first.Before(ctx, evt); err != nil {
		t.Fatalf("first recorder Before() error = %v", err)
	}

	// A new recorder simulates restarting chronos-code and resuming the same
	// persisted conversation session.
	resumed := NewTelemetryRecorder(store, "/repo", "coder")
	if err := resumed.Before(ctx, evt); err != nil {
		t.Fatalf("resumed recorder Before() error = %v", err)
	}

	var sessions, turns int
	if err := store.db.QueryRow(`SELECT count(*) FROM sessions WHERE id = 'resumed-session'`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM turns WHERE session_id = 'resumed-session'`).Scan(&turns); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if sessions != 1 || turns != 2 {
		t.Errorf("resumed telemetry counts = sessions %d, turns %d; want 1, 2", sessions, turns)
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
