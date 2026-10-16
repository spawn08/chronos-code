package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
)

func TestWorkingContextKeepsRawUserSeparateAndRefreshesTail(t *testing.T) {
	a := pipelineAgent(t)
	current := "old recall"
	a.ContextPinsFn = func(context.Context) []model.Message {
		return []model.Message{{Role: model.RoleSystem, Content: "standing"}, {Role: model.RoleSystem, TurnScoped: true, Content: current}}
	}
	o := &Orchestrator{cfg: &config.Config{Tools: config.ToolsConfig{LeanContext: true}}}
	o.installWorkingContext(a)
	pins := a.ContextPinsFn(context.Background())
	if len(pins) != 1 || pins[0].Content != "standing" {
		t.Fatalf("transient recall entered persisted pins: %v", pins)
	}
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "live request"}}}
	if err := a.Hooks.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	current = "new recall"
	if err := a.Hooks.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 || req.Messages[0].Content != "live request" || strings.Contains(req.Messages[1].Content, "old recall") || !strings.Contains(req.Messages[1].Content, "new recall") {
		t.Fatalf("tail not refreshed: %v", req.Messages)
	}
}

func TestScopedRulesActivateForSourceSubdirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".cursor", "rules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "AGENTS.md"), []byte("NESTED_RULE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cursor", "rules", "typescript.mdc"), []byte("---\nglobs: src/**/*.ts\nalwaysApply: false\n---\nTYPESCRIPT_RULE"), 0o600); err != nil {
		t.Fatal(err)
	}
	hook := &scopedRulesHook{root: root, paths: map[string][]string{}}
	if err := hook.Before(context.Background(), &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_read", Input: map[string]any{"path": "src/main.ts"}}); err != nil {
		t.Fatal(err)
	}
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "task"}}}
	if err := hook.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 || !strings.Contains(req.Messages[1].Content, "NESTED_RULE") || !strings.Contains(req.Messages[1].Content, "TYPESCRIPT_RULE") {
		t.Fatalf("rules missing: %+v", req.Messages)
	}
}
