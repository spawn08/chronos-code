package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/learning"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos/sdk/agent"
)

func TestResumeSessionUsesLatestWhenIDOmitted(t *testing.T) {
	orch := newTestOrch(t)
	first := orch.CurrentSessionID()
	if first == "" {
		t.Fatal("expected an initial session")
	}
	if _, err := orch.ResetSession(context.Background()); err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
	if orch.CurrentSessionID() == first {
		t.Fatal("ResetSession did not change session id")
	}
	got, err := orch.ResumeSession(context.Background(), first)
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if got != first || orch.CurrentSessionID() != first {
		t.Fatalf("resumed %q current %q, want %q", got, orch.CurrentSessionID(), first)
	}
}

func TestPlanModeBlocksMutatingTools(t *testing.T) {
	orch := newTestOrch(t)
	orch.SetPlanMode(true)
	hook := sessionUXHook{orchestrator: orch}
	err := hook.Before(context.Background(), &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_write", Input: map[string]any{"path": "a.go"}})
	if err == nil || !strings.Contains(err.Error(), "plan mode") {
		t.Fatalf("plan mode file_write error = %v", err)
	}
	if err := hook.Before(context.Background(), &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_read"}); err != nil {
		t.Fatalf("plan mode file_read blocked: %v", err)
	}
	orch.SetPlanMode(false)
	if err := hook.Before(context.Background(), &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_write", Input: map[string]any{"path": "a.go"}}); err != nil {
		t.Fatalf("plan mode off still blocked writes: %v", err)
	}
}

func TestUndoLastEditRestoresSnapshot(t *testing.T) {
	orch := newTestOrch(t)
	root := orch.cfg.Workspace.Root
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	hook := sessionUXHook{orchestrator: orch}
	input := map[string]any{"path": "note.txt"}
	if err := hook.Before(context.Background(), &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_write", Input: input}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := hook.After(context.Background(), &hooks.Event{Type: hooks.EventToolCallAfter, Name: "file_write", Input: input}); err != nil {
		t.Fatal(err)
	}
	got, err := orch.UndoLastEdit()
	if err != nil || got != "note.txt" {
		t.Fatalf("UndoLastEdit = %q, %v", got, err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "v1" {
		t.Fatalf("restored %q, want v1", body)
	}
}

func TestLastRouteStatusAndLearningStore(t *testing.T) {
	orch := newTestOrch(t)
	orch.recordRoute(router.Classification{Complexity: router.ComplexityLow, Kind: router.TaskKindExplain}, orch.ActiveID())
	if got := orch.LastRouteStatus(); got != "route:low/explain" {
		t.Fatalf("LastRouteStatus = %q", got)
	}
	store := orch.suggestionStore()
	if err := store.Save(&learning.Suggestion{ID: "s1", Kind: "pattern", Title: "t", YAML: "x: 1"}); err != nil {
		t.Fatal(err)
	}
	pending, err := orch.ListPendingSuggestions()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v, %v", pending, err)
	}
	if err := orch.RejectSuggestion("s1"); err != nil {
		t.Fatal(err)
	}
}

func newTestOrch(t *testing.T) *Orchestrator {
	t.Helper()
	root := t.TempDir()
	indexOnStart := false
	cfg := &config.Config{
		FileConfig: agent.FileConfig{
			Defaults: &agent.AgentConfig{Storage: agent.StorageConfig{
				Backend: "sqlite",
				DSN:     filepath.Join(root, "sessions.db"),
			}},
			Agents: []agent.AgentConfig{{
				ID:   "coder",
				Name: "Coder",
				Model: agent.ModelConfig{
					Provider: "openai",
					Model:    "gpt-4o-mini",
					APIKey:   "test-key",
				},
			}},
		},
		Workspace: config.WorkspaceConfig{Root: root, IndexOnStart: &indexOnStart},
		Learning:  config.LearningConfig{OutputDir: filepath.Join(root, "learned")},
	}
	orch, err := New(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = orch.Close() })
	return orch
}
