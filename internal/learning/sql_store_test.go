package learning

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestMigrateIsIdempotentAndConfiguresSQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "learning.db")

	store, err := OpenSQLStore(ctx, path)
	if err != nil {
		t.Fatalf("OpenSQLStore() error = %v", err)
	}
	assertSQLConfiguration(t, store)
	assertSchema(t, store)
	if err := store.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	store, err = OpenSQLStore(ctx, path)
	if err != nil {
		t.Fatalf("OpenSQLStore() after migration error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	assertSchema(t, store)
}

func TestSQLStoreWritesAndStats(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	now := time.Date(2026, 9, 2, 10, 11, 12, 123, time.FixedZone("test", 2*60*60))
	ended := now.Add(time.Minute)

	if err := store.CreateSession(ctx, Session{
		ID: "session-1", RepoPath: "/repo", StartedAt: now, EndedAt: &ended,
		Model: "model", Turns: 1, InputTokens: 10, OutputTokens: 20, CostUSD: 0.25,
	}); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if err := store.AppendTurn(ctx, Turn{
		ID: "turn-1", SessionID: "session-1", Role: "user", Content: "fix it", Timestamp: now,
	}); err != nil {
		t.Fatalf("AppendTurn() error = %v", err)
	}
	if err := store.RecordToolCall(ctx, ToolCall{
		ID: "call-1", TurnID: "turn-1", Name: "file_read", Input: `{"path":"a.go"}`,
		Output: "package a", Duration: 1500 * time.Millisecond, Timestamp: now,
	}); err != nil {
		t.Fatalf("RecordToolCall() error = %v", err)
	}
	if err := store.RecordOutcome(ctx, Outcome{
		TurnID: "turn-1", Kind: "edited", UserEdit: "small correction", Timestamp: ended,
	}); err != nil {
		t.Fatalf("RecordOutcome() error = %v", err)
	}

	var repoPath, startedAt, endedAt, model string
	var turns, inputTokens, outputTokens int64
	var cost float64
	err := store.db.QueryRowContext(ctx, `
		SELECT repo_path, started_at, ended_at, model, turns, input_tokens, output_tokens, cost_usd
		FROM sessions WHERE id = 'session-1'`).Scan(
		&repoPath, &startedAt, &endedAt, &model, &turns, &inputTokens, &outputTokens, &cost,
	)
	if err != nil {
		t.Fatalf("query session: %v", err)
	}
	if repoPath != "/repo" || startedAt != timestamp(now) || endedAt != timestamp(ended) || model != "model" ||
		turns != 1 || inputTokens != 10 || outputTokens != 20 || cost != 0.25 {
		t.Errorf("stored session = %q %q %q %q %d %d %d %f", repoPath, startedAt, endedAt, model, turns, inputTokens, outputTokens, cost)
	}

	var name, input, output, kind, userEdit string
	var durationMS int64
	if err := store.db.QueryRowContext(ctx, `
		SELECT tc.name, tc.input, tc.output, tc.duration_ms, o.kind, o.user_edit
		FROM tool_calls tc JOIN outcomes o ON o.turn_id = tc.turn_id
		WHERE tc.id = 'call-1'`).Scan(&name, &input, &output, &durationMS, &kind, &userEdit); err != nil {
		t.Fatalf("query related writes: %v", err)
	}
	if name != "file_read" || input != `{"path":"a.go"}` || output != "package a" || durationMS != 1500 || kind != "edited" || userEdit != "small correction" {
		t.Errorf("stored tool call/outcome = %q %q %q %d %q %q", name, input, output, durationMS, kind, userEdit)
	}

	want := &StatsReport{Sessions: 1, Turns: 1, ToolCalls: 1, Outcomes: 1, Edited: 1}
	got, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Stats() = %+v, want %+v", got, want)
	}
}

func TestSQLStoreEnforcesForeignKeys(t *testing.T) {
	store := openTestSQLStore(t)
	err := store.AppendTurn(context.Background(), Turn{
		ID: "orphan", SessionID: "missing", Role: "user", Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatal("AppendTurn() with missing session error = nil")
	}

	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.Turns != 0 {
		t.Fatalf("Stats().Turns = %d, want 0", stats.Turns)
	}
}

func TestSQLStoreConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	const writers = 32

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sessionID := fmt.Sprintf("session-%02d", i)
			turnID := fmt.Sprintf("turn-%02d", i)
			if err := store.CreateSession(ctx, Session{ID: sessionID, RepoPath: "/repo", StartedAt: time.Now()}); err != nil {
				errs <- err
				return
			}
			if err := store.AppendTurn(ctx, Turn{ID: turnID, SessionID: sessionID, Role: "user", Timestamp: time.Now()}); err != nil {
				errs <- err
				return
			}
			if err := store.RecordToolCall(ctx, ToolCall{ID: fmt.Sprintf("call-%02d", i), TurnID: turnID, Name: "file_read", Timestamp: time.Now()}); err != nil {
				errs <- err
				return
			}
			if err := store.RecordOutcome(ctx, Outcome{TurnID: turnID, Kind: "accepted", Timestamp: time.Now()}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write: %v", err)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	want := &StatsReport{Sessions: writers, Turns: writers, ToolCalls: writers, Outcomes: writers, Accepted: writers}
	if !reflect.DeepEqual(stats, want) {
		t.Errorf("Stats() = %+v, want %+v", stats, want)
	}
}

func TestSQLStoreContextAndClose(t *testing.T) {
	store := openTestSQLStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Stats(ctx); err == nil {
		t.Fatal("Stats() with canceled context error = nil")
	}
	if err := store.Close(ctx); err == nil {
		t.Fatal("Close() with canceled context error = nil")
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestSegmentsPreserveTurnOrderAndOutcomeLinkage(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession(ctx, Session{ID: "session", RepoPath: "/repo", StartedAt: base}); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	turns := []Turn{
		{ID: "u2", SessionID: "session", Role: "user", Content: "second", Timestamp: base.Add(4 * time.Minute)},
		{ID: "a1", SessionID: "session", Role: "assistant", Content: "first answer", Timestamp: base.Add(2 * time.Minute)},
		{ID: "u1", SessionID: "session", Role: "user", Content: "first", Timestamp: base.Add(time.Minute)},
		{ID: "a2", SessionID: "session", Role: "assistant", Content: "second answer", Timestamp: base.Add(5 * time.Minute)},
	}
	for _, turn := range turns {
		if err := store.AppendTurn(ctx, turn); err != nil {
			t.Fatalf("AppendTurn(%q) error = %v", turn.ID, err)
		}
	}
	for _, call := range []ToolCall{
		{ID: "call-2", TurnID: "a1", Name: "test", Timestamp: base.Add(3 * time.Minute)},
		{ID: "call-1", TurnID: "a1", Name: "read", Timestamp: base.Add(2 * time.Minute)},
	} {
		if err := store.RecordToolCall(ctx, call); err != nil {
			t.Fatalf("RecordToolCall(%q) error = %v", call.ID, err)
		}
	}
	if err := store.RecordOutcome(ctx, Outcome{TurnID: "a1", Kind: "accepted", Timestamp: base.Add(3 * time.Minute)}); err != nil {
		t.Fatalf("RecordOutcome() error = %v", err)
	}

	segments, err := store.Segments(ctx, "/repo")
	if err != nil {
		t.Fatalf("Segments() error = %v", err)
	}
	if len(segments) != 2 {
		t.Fatalf("Segments() returned %d segments, want 2", len(segments))
	}
	if got := []string{segments[0].Turns[0].ID, segments[0].Turns[1].ID}; !reflect.DeepEqual(got, []string{"u1", "a1"}) {
		t.Errorf("first segment turn order = %v, want [u1 a1]", got)
	}
	if got := []string{segments[0].ToolCalls[0].Name, segments[0].ToolCalls[1].Name}; !reflect.DeepEqual(got, []string{"read", "test"}) {
		t.Errorf("first segment tool order = %v, want [read test]", got)
	}
	if segments[0].Outcome == nil || segments[0].Outcome.TurnID != "a1" || segments[0].Outcome.Kind != "accepted" {
		t.Errorf("first segment outcome = %+v, want accepted outcome linked to a1", segments[0].Outcome)
	}
	if segments[1].Trigger.ID != "u2" || segments[1].Outcome != nil {
		t.Errorf("second segment = %+v, want trigger u2 without outcome", segments[1])
	}
}

func TestSegmentsOrderFractionalTimestampsChronologically(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession(ctx, Session{ID: "session", RepoPath: "/repo", StartedAt: base}); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	for _, turn := range []Turn{
		{ID: "a-answer", SessionID: "session", Role: "assistant", Content: "answer", Timestamp: base.Add(100 * time.Millisecond)},
		{ID: "z-user", SessionID: "session", Role: "user", Content: "request", Timestamp: base},
	} {
		if err := store.AppendTurn(ctx, turn); err != nil {
			t.Fatalf("AppendTurn(%q) error = %v", turn.ID, err)
		}
	}
	for _, call := range []ToolCall{
		{ID: "a-later", TurnID: "a-answer", Name: "test", Timestamp: base.Add(200 * time.Millisecond)},
		{ID: "z-earlier", TurnID: "a-answer", Name: "read", Timestamp: base.Add(100 * time.Millisecond)},
	} {
		if err := store.RecordToolCall(ctx, call); err != nil {
			t.Fatalf("RecordToolCall(%q) error = %v", call.ID, err)
		}
	}

	segments, err := store.Segments(ctx, "/repo")
	if err != nil {
		t.Fatalf("Segments() error = %v", err)
	}
	if len(segments) != 1 {
		t.Fatalf("Segments() returned %d segments, want 1", len(segments))
	}
	if got := []string{segments[0].Turns[0].ID, segments[0].Turns[1].ID}; !reflect.DeepEqual(got, []string{"z-user", "a-answer"}) {
		t.Errorf("segment turn order = %v, want [z-user a-answer]", got)
	}
	if got := []string{segments[0].ToolCalls[0].Name, segments[0].ToolCalls[1].Name}; !reflect.DeepEqual(got, []string{"read", "test"}) {
		t.Errorf("segment tool order = %v, want [read test]", got)
	}
}

func TestCandidatePersistenceIsIdempotentAndOrdered(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	for group, trigger := range []string{"zeta task", "alpha task"} {
		for i := 0; i < MinimumCandidateCount; i++ {
			sessionID := fmt.Sprintf("session-%d-%d", group, i)
			turnID := fmt.Sprintf("turn-%d-%d", group, i)
			at := base.Add(time.Duration(group*10+i) * time.Minute)
			if err := store.CreateSession(ctx, Session{ID: sessionID, RepoPath: "/repo", StartedAt: at}); err != nil {
				t.Fatalf("CreateSession() error = %v", err)
			}
			if err := store.AppendTurn(ctx, Turn{ID: turnID, SessionID: sessionID, Role: "user", Content: trigger, Timestamp: at}); err != nil {
				t.Fatalf("AppendTurn() error = %v", err)
			}
			if err := store.RecordOutcome(ctx, Outcome{TurnID: turnID, Kind: "accepted", Timestamp: at}); err != nil {
				t.Fatalf("RecordOutcome() error = %v", err)
			}
		}
	}

	first, err := store.ExtractCandidates(ctx, "/repo")
	if err != nil {
		t.Fatalf("ExtractCandidates() error = %v", err)
	}
	second, err := store.ExtractCandidates(ctx, "/repo")
	if err != nil {
		t.Fatalf("ExtractCandidates() repeated error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("repeated ExtractCandidates() changed results:\nfirst  = %+v\nsecond = %+v", first, second)
	}
	if len(second) != 2 || second[0].TriggerHash > second[1].TriggerHash {
		t.Fatalf("ExtractCandidates() = %+v, want two candidates ordered by hash", second)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.Patterns != 2 {
		t.Errorf("Stats().Patterns = %d, want 2 after repeated extraction", stats.Patterns)
	}
}

func openTestSQLStore(t *testing.T) *SQLStore {
	t.Helper()
	store, err := OpenSQLStore(context.Background(), filepath.Join(t.TempDir(), "learning.db"))
	if err != nil {
		t.Fatalf("OpenSQLStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

func assertSQLConfiguration(t *testing.T, store *SQLStore) {
	t.Helper()
	var journalMode string
	var busyTimeout, foreignKeys int
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if err := store.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if err := store.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if journalMode != "wal" || busyTimeout != 5000 || foreignKeys != 1 {
		t.Errorf("SQLite configuration = journal_mode:%q busy_timeout:%d foreign_keys:%d", journalMode, busyTimeout, foreignKeys)
	}
}

func assertSchema(t *testing.T, store *SQLStore) {
	t.Helper()
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}

	rows, err := store.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	got := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	for _, name := range []string{"sessions", "turns", "tool_calls", "outcomes", "patterns"} {
		if !got[name] {
			t.Errorf("schema missing table %q", name)
		}
	}
}
