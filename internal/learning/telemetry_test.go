package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestAsyncTelemetrySnapshotsAndDrains(t *testing.T) {
	store := openTestSQLStore(t)
	ctx, cancel := context.WithCancel(storage.WithSession(context.Background(), "snapshot"))
	defer cancel()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := NewAsyncTelemetryRecorder(store, "/repo", "coder", TelemetryRecorderOptions{})
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	input := map[string]any{"prompt": []string{"input-secret"}}
	output := &model.ChatResponse{Content: "output-secret", Usage: model.Usage{PromptTokens: 7, CompletionTokens: 11}}
	inputHash, _ := eventFingerprint(input)
	outputHash, _ := eventFingerprint(output)
	evt := &hooks.Event{Type: hooks.EventModelCallBefore, Name: "model", Input: input, Metadata: map[string]any{"secret": "metadata-secret"}}
	if err := r.Before(ctx, evt); err != nil {
		t.Fatal(err)
	}
	waitTelemetryDBWait(t, store)
	callID := evt.Metadata[telemetryCorrelationKey]
	evt.Type, evt.Output = hooks.EventModelCallAfter, output
	if err := r.After(ctx, evt); err != nil {
		t.Fatal(err)
	}
	// Mutating all caller-owned data after the hook returns must be safe even
	// while the worker is using the snapshot, including nested input and usage.
	mutated := make(chan struct{})
	go func() {
		defer close(mutated)
		for i := 0; i < 1000; i++ {
			input["prompt"].([]string)[0] = "changed"
			output.Content = "changed"
			output.Usage.PromptTokens = i
			evt.Metadata[telemetryCorrelationKey] = "changed"
			evt.Name = "changed"
		}
	}()
	cancel() // Accepted telemetry is independent of the agent call's lifetime.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-mutated
	var content string
	var turns, in, out int
	if err := store.db.QueryRow(`SELECT content FROM turns`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["name"] != "model" || payload["correlation_id"] != callID || payload["input_hash"] != inputHash || payload["output_hash"] != outputHash || payload["state"] != "completed" {
		t.Fatalf("snapshot changed: %s", content)
	}
	if strings.Contains(content, "secret") {
		t.Fatalf("raw secret in telemetry: %s", content)
	}
	if err := store.db.QueryRow(`SELECT turns, input_tokens, output_tokens FROM sessions WHERE id = 'snapshot'`).Scan(&turns, &in, &out); err != nil {
		t.Fatal(err)
	}
	if turns != 1 || in != 7 || out != 11 {
		t.Fatalf("totals = %d/%d/%d", turns, in, out)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Ping(); err != nil {
		t.Fatalf("recorder closed borrowed store: %v", err)
	}
}

func TestAsyncTelemetrySaturationAndClose(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := NewAsyncTelemetryRecorder(store, "/repo", "coder", TelemetryRecorderOptions{QueueCapacity: 1})
	t.Cleanup(func() { _ = r.Close(ctx) })
	record := func() {
		t.Helper()
		if err := r.After(ctx, &hooks.Event{Type: hooks.EventToolCallAfter, Name: "tool", Input: "input-secret", Output: "output-secret"}); err != nil {
			t.Fatalf("optional telemetry aborted hook: %v", err)
		}
	}
	record()
	waitTelemetryDBWait(t, store)
	record()
	record() // Full queue: must return without waiting for the DB connection.
	if got := r.Stats(); got.Accepted != 2 || got.Dropped != 1 || got.Processed != 0 {
		t.Fatalf("saturated stats = %+v", got)
	}
	deadline, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := r.Flush(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Flush deadline = %v", err)
	}
	if err := r.Close(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close deadline = %v", err)
	}
	record() // Shutdown rejects new work without aborting the agent.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := r.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.Stats(); got.Accepted != 2 || got.Processed != 2 || got.Dropped != 2 || got.Errors != 0 {
		t.Fatalf("closed stats = %+v", got)
	}
	var calls int
	if err := store.db.QueryRow(`SELECT count(*) FROM tool_calls WHERE input LIKE 'sha256:%' AND output LIKE 'sha256:%'`).Scan(&calls); err != nil || calls != 2 {
		t.Fatalf("drained tool calls = %d, err = %v", calls, err)
	}
	if len(r.calls) != 0 {
		t.Fatal("Close retained correlation state")
	}
}

func TestAsyncTelemetryErrorsAreObservableAndNonfatal(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	if err := store.Close(ctx); err != nil {
		t.Fatal(err)
	}
	r := NewAsyncTelemetryRecorder(store, "/repo", "coder", TelemetryRecorderOptions{})
	t.Cleanup(func() { _ = r.Close(ctx) })
	for _, typ := range []hooks.EventType{hooks.EventModelCallBefore, hooks.EventToolCallBefore} {
		if err := r.Before(ctx, &hooks.Event{Type: typ, Name: "call"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Before(ctx, &hooks.Event{Type: hooks.EventToolCallBefore, Input: make(chan int)}); err != nil {
		t.Fatalf("snapshot error aborted hook: %v", err)
	}
	if err := r.Flush(ctx); err == nil {
		t.Fatal("Flush did not report errors")
	}
	for i := 0; i < 2; i++ {
		if err := r.Close(ctx); err == nil {
			t.Fatal("Close did not report retained error")
		}
	}
	if got := r.Stats(); got.Accepted != 2 || got.Processed != 2 || got.Errors != 3 || got.Dropped != 0 {
		t.Fatalf("error stats = %+v", got)
	}
}

func TestAsyncTelemetryWriteTimeout(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := NewAsyncTelemetryRecorder(store, "/repo", "coder", TelemetryRecorderOptions{WriteTimeout: 20 * time.Millisecond})
	if err := r.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write timeout = %v", err)
	}
	if got := r.Stats(); got.Errors != 1 || got.Processed != 1 {
		t.Fatalf("timeout stats = %+v", got)
	}
}

func TestAsyncTelemetryBoundedStateAndDuplicateCompletion(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	r := NewAsyncTelemetryRecorder(store, "/repo", "coder", TelemetryRecorderOptions{QueueCapacity: 64, CompletedCallLimit: 3, PendingCallLimit: 2})
	t.Cleanup(func() { _ = r.Close(ctx) })
	var last *hooks.Event
	for i := 0; i < 20; i++ {
		last = &hooks.Event{Type: hooks.EventModelCallAfter, Output: &model.ChatResponse{Usage: model.Usage{PromptTokens: 1}}}
		if err := r.After(ctx, last); err != nil {
			t.Fatal(err)
		}
		if err := r.Before(storage.WithSession(ctx, fmt.Sprint(i)), &hooks.Event{Type: hooks.EventToolCallBefore}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 5 {
		t.Fatalf("retained calls = %d, want 3 completed + 2 pending", len(r.calls))
	}
	for i := 0; i < 10; i++ {
		if err := r.After(ctx, last); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var turns, tokens int
	if err := store.db.QueryRow(`SELECT turns, input_tokens FROM sessions WHERE id = 'coder'`).Scan(&turns, &tokens); err != nil {
		t.Fatal(err)
	}
	if turns != 20 || tokens != 20 || len(r.calls) != 5 {
		t.Fatalf("duplicate changed totals/state: turns=%d tokens=%d retained=%d", turns, tokens, len(r.calls))
	}
}

func TestAsyncTelemetryConcurrentHooksFlushAndClose(t *testing.T) {
	ctx := context.Background()
	r := NewAsyncTelemetryRecorder(openTestSQLStore(t), "/repo", "coder", TelemetryRecorderOptions{QueueCapacity: 8})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := r.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter}); err != nil {
					t.Error(err)
				}
				_ = r.Stats()
				if err := r.Flush(ctx); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Close(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := r.Stats(); got.Accepted+got.Dropped != 160 || got.Processed != got.Accepted || got.Errors != 0 {
		t.Fatalf("concurrent stats = %+v", got)
	}
}

func waitTelemetryDBWait(t *testing.T, store *SQLStore) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for store.db.Stats().WaitCount == 0 {
		if time.Now().After(deadline) {
			t.Fatal("telemetry worker did not reach database")
		}
		time.Sleep(time.Millisecond)
	}
}
