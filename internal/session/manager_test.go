package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos/storage"
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

func TestNewSessionID(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := NewSessionID()
		if len(id) == 0 {
			t.Fatalf("NewSessionID returned empty string")
		}
		if id[:5] != "sess_" {
			t.Fatalf("NewSessionID() = %q, want prefix \"sess_\"", id)
		}
		if seen[id] {
			t.Fatalf("NewSessionID produced duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestManagerEnsure(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m := NewManager(store, ":memory:")

	tests := []struct {
		name      string
		sessionID string
		agentID   string
	}{
		{name: "creates new session", sessionID: "sess_a", agentID: "agent-1"},
		{name: "idempotent on same agent", sessionID: "sess_a", agentID: "agent-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := m.Ensure(ctx, tt.sessionID, tt.agentID); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			got, err := store.GetSession(ctx, tt.sessionID)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			if got.AgentID != tt.agentID {
				t.Fatalf("AgentID = %q, want %q", got.AgentID, tt.agentID)
			}
			if got.Status != "running" {
				t.Fatalf("Status = %q, want %q", got.Status, "running")
			}
		})
	}

	// Ensure with a different agentID for the same session id must error.
	if err := m.Ensure(ctx, "sess_a", "agent-2"); err == nil {
		t.Fatalf("Ensure with mismatched agent: expected error, got nil")
	}
}

func TestManagerListAndLatest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m := NewManager(store, ":memory:")

	// No sessions yet: Latest should return (nil, nil).
	latest, err := m.Latest(ctx, "agent-1")
	if err != nil {
		t.Fatalf("Latest on empty agent: %v", err)
	}
	if latest != nil {
		t.Fatalf("Latest on empty agent = %+v, want nil", latest)
	}

	base := time.Now().Add(-time.Hour)
	ids := []string{"sess_1", "sess_2", "sess_3"}
	for i, id := range ids {
		sess := &storage.Session{
			ID:        id,
			AgentID:   "agent-1",
			Status:    "running",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := store.CreateSession(ctx, sess); err != nil {
			t.Fatalf("CreateSession(%s): %v", id, err)
		}
	}

	list, err := m.List(ctx, "agent-1", 10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != len(ids) {
		t.Fatalf("List returned %d sessions, want %d", len(list), len(ids))
	}
	// ListSessions orders by created_at DESC, so the most recently created
	// session (sess_3) should be first.
	if list[0].ID != "sess_3" {
		t.Fatalf("List[0].ID = %q, want %q", list[0].ID, "sess_3")
	}

	latest, err = m.Latest(ctx, "agent-1")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest == nil || latest.ID != "sess_3" {
		t.Fatalf("Latest = %+v, want session sess_3", latest)
	}
}

func TestManagerTouch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m := NewManager(store, ":memory:")

	sess := &storage.Session{
		ID:        "sess_touch",
		AgentID:   "agent-1",
		Status:    "running",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := m.Touch(ctx, "sess_touch", "completed"); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, err := store.GetSession(ctx, "sess_touch")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Status != "completed" {
		t.Fatalf("Status = %q, want %q", got.Status, "completed")
	}

	// Touch with empty status should leave status unchanged.
	if err := m.Touch(ctx, "sess_touch", ""); err != nil {
		t.Fatalf("Touch (no status): %v", err)
	}
	got2, err := store.GetSession(ctx, "sess_touch")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got2.Status != "completed" {
		t.Fatalf("Status after no-op touch = %q, want %q", got2.Status, "completed")
	}
}

func TestManagerExport(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m := NewManager(store, ":memory:")

	sess := &storage.Session{
		ID:        "sess_export",
		AgentID:   "agent-1",
		Status:    "running",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	evt := &storage.Event{
		ID: "evt_1", SessionID: "sess_export", SeqNum: 1, Type: "test",
		Payload: map[string]any{"k": "v"}, CreatedAt: time.Now(),
	}
	if err := store.AppendEvent(ctx, evt); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	cp := &storage.Checkpoint{
		ID: "cp_1", SessionID: "sess_export", RunID: "run_1", NodeID: "node_1",
		State: map[string]any{"s": 1}, SeqNum: 1, CreatedAt: time.Now(),
	}
	if err := store.SaveCheckpoint(ctx, cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	tr := &storage.Trace{
		ID: "tr_1", SessionID: "sess_export", Name: "step", Kind: "node",
		StartedAt: time.Now(),
	}
	if err := store.InsertTrace(ctx, tr); err != nil {
		t.Fatalf("InsertTrace: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "export.json")

	if err := m.Export(ctx, "sess_export", path); err != nil {
		t.Fatalf("Export: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var exported ExportedSession
	if err := json.Unmarshal(data, &exported); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if exported.Session == nil || exported.Session.ID != "sess_export" {
		t.Fatalf("exported session = %+v, want ID sess_export", exported.Session)
	}
	if len(exported.Events) != 1 || exported.Events[0].ID != "evt_1" {
		t.Fatalf("exported events = %+v", exported.Events)
	}
	if len(exported.Checkpoints) != 1 || exported.Checkpoints[0].ID != "cp_1" {
		t.Fatalf("exported checkpoints = %+v", exported.Checkpoints)
	}
	if len(exported.Traces) != 1 || exported.Traces[0].ID != "tr_1" {
		t.Fatalf("exported traces = %+v", exported.Traces)
	}
}

func TestManagerDelete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "sessions.db")

	store, err := sqlite.New(dsn)
	if err != nil {
		t.Fatalf("sqlite.New(%s): %v", dsn, err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	m := NewManager(store, dsn)

	sess := &storage.Session{
		ID:        "sess_delete",
		AgentID:   "agent-1",
		Status:    "running",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	evt := &storage.Event{
		ID: "evt_del", SessionID: "sess_delete", SeqNum: 1, Type: "test",
		Payload: map[string]any{"k": "v"}, CreatedAt: time.Now(),
	}
	if err := store.AppendEvent(ctx, evt); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	if err := m.Delete(ctx, "sess_delete"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := store.GetSession(ctx, "sess_delete"); err == nil {
		t.Fatalf("GetSession after Delete: expected error, got nil")
	}

	events, err := store.ListEvents(ctx, "sess_delete", 0)
	if err != nil {
		t.Fatalf("ListEvents after Delete: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("ListEvents after Delete = %+v, want empty", events)
	}

	// Deleting again (rows already gone) must not error.
	if err := m.Delete(ctx, "sess_delete"); err != nil {
		t.Fatalf("Delete (idempotent): %v", err)
	}
}
