package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

func TestSetupGraphPinsIndexGuidanceForCustomAgents(t *testing.T) {
	root, _ := runtimeTestEnvironment(t)
	cfg := runtimeTestConfig(root)
	a := &agent.Agent{
		SystemPrompt: "Existing user-authored prompt.",
		Tools:        tool.NewRegistry(),
		ContextPinsFn: func(context.Context) []model.Message {
			return []model.Message{{Role: model.RoleSystem, Content: "Existing pin."}}
		},
	}
	store, scope := setupGraph(context.Background(), cfg, t.TempDir(), "", "", map[string]*agent.Agent{"custom": a})
	if scope == nil || store == nil {
		t.Fatal("code index unavailable")
	}
	t.Cleanup(func() { _ = scope.Close() })
	if a.SystemPrompt != "Existing user-authored prompt." {
		t.Fatal("custom prompt overwritten")
	}
	pins := a.ContextPinsFn(context.Background())
	if len(pins) != 2 || pins[0].Content != "Existing pin." || pins[1].Role != model.RoleSystem {
		t.Fatalf("pins = %#v", pins)
	}
	for _, text := range []string{"codebase_context", "codebase_search", "resolve_symbol", "literal text", "[Repository context]", "unavailable", "impact_analysis"} {
		if !strings.Contains(pins[1].Content, text) {
			t.Errorf("runtime index guidance missing %q", text)
		}
	}
	for _, name := range []string{"codebase_context", "codebase_search", "resolve_symbol", "impact_analysis"} {
		if _, ok := a.Tools.Get(name); !ok {
			t.Errorf("guidance names unregistered tool %q", name)
		}
	}
}
