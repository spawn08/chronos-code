package orchestrator

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/storage"
	"github.com/spawn08/chronos/storage/adapters/sqlite"
)

func TestSessionHistorySurvivesStorageRestart(t *testing.T) {
	ctx := storage.WithTenant(context.Background(), "history-tenant")
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const id = "history-restart"
	if err := store.CreateSession(ctx, &storage.Session{ID: id, AgentID: "coder", Status: "running", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(ctx, &storage.Event{ID: "message", SessionID: id, SeqNum: 1, Type: "chat_message", Payload: map[string]any{"role": "user", "content": "saved question"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	o := &Orchestrator{store: store}
	messages, err := o.SessionHistory(ctx, id)
	want := []model.Message{{Role: model.RoleUser, Content: "saved question"}}
	if err != nil || !reflect.DeepEqual(messages, want) {
		t.Fatalf("reopened history = %#v, %v", messages, err)
	}
	messages, err = o.SessionHistory(storage.WithTenant(context.Background(), "other-tenant"), id)
	if err != nil || len(messages) != 0 {
		t.Fatalf("other tenant history = %#v, %v", messages, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := o.SessionHistory(ctx, id); err == nil {
		t.Fatal("storage read failure was hidden")
	}
}

func TestSessionHistoryUnavailableStorage(t *testing.T) {
	for _, o := range []*Orchestrator{nil, {}} {
		if _, err := o.SessionHistory(context.Background(), "session"); err == nil {
			t.Fatal("unavailable storage did not report an error")
		}
	}
}
