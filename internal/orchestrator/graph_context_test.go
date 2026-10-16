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
	for _, text := range []string{"codebase_context", "codebase_search", "resolve_symbol", "literal text", "[Repository context]", "unavailable", "impact_analysis", "latest user request", "current working tree", "cost-analysis", "Commit messages", "unverified"} {
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

func TestSetupGraphKeepsEvidenceGuidanceWhenIndexUnavailable(t *testing.T) {
	root, _ := runtimeTestEnvironment(t)
	cfg := runtimeTestConfig(root)
	agents := make(map[string]*agent.Agent)
	for _, id := range []string{"chronos-code", "researcher", "custom"} {
		agents[id] = &agent.Agent{
			SystemPrompt: "Existing agent prompt.",
			Tools:        tool.NewRegistry(),
			ContextPinsFn: func(context.Context) []model.Message {
				return []model.Message{{Role: model.RoleSystem, Content: "Existing pin."}}
			},
		}
	}
	// An empty data directory fails before any index tools can be installed.
	store, scope := setupGraph(context.Background(), cfg, "", "", "", agents)
	if store != nil || scope != nil {
		if scope != nil {
			_ = scope.Close()
		}
		t.Fatal("expected index startup to fail")
	}
	for id, a := range agents {
		pins := a.ContextPinsFn(context.Background())
		if a.SystemPrompt != "Existing agent prompt." || len(pins) != 2 || pins[0].Content != "Existing pin." || pins[1].Role != model.RoleSystem || pins[1].TurnScoped {
			t.Fatalf("%s lost its prompt or standing context: %#v", id, pins)
		}
		if !strings.Contains(pins[1].Content, "current working tree") || !strings.Contains(pins[1].Content, "direct source tools") {
			t.Errorf("%s lost current-state verification or index fallback guidance", id)
		}
	}
}
