package orchestrator

import (
	"context"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos-code/internal/router"
)

func TestBuildRuntimeCapabilityManifestScopesLiveCapabilities(t *testing.T) {
	coderTools := tool.NewRegistry()
	coderTools.Register(&tool.Definition{Name: "file_write", Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	coderTools.Register(&tool.Definition{Name: "lsp_diagnostics", Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	coderTools.Register(&tool.Definition{Name: "mcp__github__search", Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	coderTools.Register(&tool.Definition{Name: "denied", Permission: tool.PermDeny, Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	readerTools := tool.NewRegistry()
	readerTools.Register(&tool.Definition{Name: "file_read", Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }})

	manifest := buildRuntimeCapabilityManifest(map[string]*agent.Agent{
		"coder":  {ID: "coder", Tools: coderTools},
		"reader": {ID: "reader", Tools: readerTools},
	}, true)
	available := config.CapabilityManifest{Capabilities: manifest.Capabilities}
	for _, requirement := range []config.Capability{
		{Name: capabilityGraphCode},
		{Name: capabilityPlanMode},
		{Name: capabilityLSPTools},
		{Name: capabilityToolPrefix + "file_write", Agent: "coder"},
		{Name: capabilityWriteFiles, Agent: "coder"},
		{Name: capabilityMCPPrefix + "github", Agent: "coder"},
		{Name: capabilityToolPrefix + "file_read", Agent: "reader"},
	} {
		if err := (config.CapabilityManifest{Capabilities: []config.Capability{requirement}}).Validate(available); err != nil {
			t.Errorf("expected capability %+v: %v", requirement, err)
		}
	}
	for _, requirement := range []config.Capability{
		{Name: capabilityToolPrefix + "file_write", Agent: "reader"},
		{Name: capabilityToolPrefix + "denied", Agent: "coder"},
		{Name: capabilityClosedLoopPPD},
	} {
		if err := (config.CapabilityManifest{Capabilities: []config.Capability{requirement}}).Validate(available); err == nil {
			t.Errorf("unexpected capability %+v", requirement)
		}
	}
}

func TestValidateRuntimeCapabilitiesRejectsUninstalledConfiguredTool(t *testing.T) {
	cfg := &config.Config{FileConfig: agent.FileConfig{Agents: []agent.AgentConfig{{
		ID: "coder", Tools: []agent.ToolConfig{{Name: "update_plan", Description: "export-only placeholder"}},
	}}}}
	agents := map[string]*agent.Agent{"coder": {ID: "coder", Tools: tool.NewRegistry()}}

	_, _, err := validateRuntimeCapabilities(cfg, agents, false, nil)
	if err == nil || !strings.Contains(err.Error(), `"tool:update_plan"`) {
		t.Fatalf("validateRuntimeCapabilities() error = %v, want unavailable configured tool", err)
	}
}

func TestEmbeddedAgentTemplatesOnlyDeclareSupportedTools(t *testing.T) {
	entries, err := fs.ReadDir(defaults.FS, "agents")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := defaults.ReadFile("agents/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		var cfg agent.AgentConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		for _, configuredTool := range cfg.Tools {
			if !isConfiguredToolSupported(configuredTool.Name) {
				t.Errorf("agents/%s declares unavailable tool %q", entry.Name(), configuredTool.Name)
			}
		}
	}
}

func TestLegacySemanticSearchConfigExplainsMigration(t *testing.T) {
	cfg := &config.Config{FileConfig: agent.FileConfig{Agents: []agent.AgentConfig{{
		ID: "chronos-code", Tools: []agent.ToolConfig{{Name: "semantic_search"}},
	}}}}
	_, _, err := validateRuntimeCapabilities(cfg, nil, false, nil)
	if err == nil || !strings.Contains(err.Error(), "remove the legacy semantic_search entry") {
		t.Fatalf("legacy configuration error = %v", err)
	}
}

func TestValidateRuntimeCapabilitiesWarnsForOptionalRequirement(t *testing.T) {
	cfg := &config.Config{RuntimeCaps: config.CapabilityManifest{Capabilities: []config.Capability{{
		Name: "lsp:tools", Optional: true,
	}}}}

	_, warnings, err := validateRuntimeCapabilities(cfg, nil, false, nil)
	if err != nil {
		t.Fatalf("validateRuntimeCapabilities() error = %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"lsp:tools"`) {
		t.Fatalf("warnings = %v, want optional LSP warning", warnings)
	}
}

func TestValidateRuntimeCapabilitiesRejectsEnabledPPD(t *testing.T) {
	routingConfig := &router.Config{PPD: router.PPDConfig{Mode: router.PPDModeEnabled}}

	_, _, err := validateRuntimeCapabilities(&config.Config{}, nil, false, routingConfig)
	if err == nil || !strings.Contains(err.Error(), capabilityClosedLoopPPD) {
		t.Fatalf("validateRuntimeCapabilities() error = %v, want closed-loop PPD requirement", err)
	}
}

func TestNewRejectsUnavailableConfiguredToolDuringStartup(t *testing.T) {
	root := t.TempDir()
	indexOnStart := false
	cfg := &config.Config{
		FileConfig: agent.FileConfig{
			Defaults: &agent.AgentConfig{Storage: agent.StorageConfig{Backend: "sqlite", DSN: root + "/sessions.db"}},
			Agents: []agent.AgentConfig{{
				ID:    "coder",
				Model: agent.ModelConfig{Provider: "openai", Model: "gpt-4o-mini", APIKey: "test-key"},
				Tools: []agent.ToolConfig{{Name: "update_plan", Description: "not installed by Chronos Code"}},
			}},
		},
		Workspace: config.WorkspaceConfig{Root: root, IndexOnStart: &indexOnStart},
		Learning:  config.LearningConfig{Enabled: false},
	}

	orch, err := New(context.Background(), cfg, "")
	if orch != nil {
		_ = orch.Close()
		t.Fatal("New() returned an orchestrator for an unavailable required tool")
	}
	if err == nil || !strings.Contains(err.Error(), `"tool:update_plan"`) {
		t.Fatalf("New() error = %v, want startup capability failure", err)
	}
}

func TestEmbeddedDefaultsPassRuntimeCapabilityValidation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Chdir(root)
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	indexOnStart := false
	cfg.Workspace.Root = root
	cfg.Workspace.IndexOnStart = &indexOnStart
	cfg.Learning.Enabled = false
	cfg.Defaults.Storage = agent.StorageConfig{Backend: "sqlite", DSN: root + "/sessions.db"}

	orch, err := New(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("New() with embedded defaults error = %v", err)
	}
	t.Cleanup(func() { _ = orch.Close() })
	if len(orch.capabilities.Capabilities) == 0 {
		t.Fatal("runtime capability manifest is empty")
	}
}

func TestWithToolPhaseSelectsBoundedSchemasWithoutMutatingRegistry(t *testing.T) {
	registry := tool.NewRegistry()
	for _, name := range []string{
		"file_write", "file_read", "file_grep", "graph_callers", "shell", "spawn_subagent", "unrelated",
	} {
		registry.Register(&tool.Definition{Name: name, Description: name, Parameters: map[string]any{"type": "object"}})
	}

	ctx := WithToolPhase(context.Background(), ToolPhaseDiscover, registry)
	got := toolNames(agent.ToolDefinitions(ctx, registry.List()))
	want := []string{"file_grep", "file_read", "graph_callers"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discover tools = %v, want %v", got, want)
	}
	if got := toolNames(registry.List()); !reflect.DeepEqual(got, []string{
		"file_grep", "file_read", "file_write", "graph_callers", "shell", "spawn_subagent", "unrelated",
	}) {
		t.Fatalf("registry changed after phase selection: %v", got)
	}
	if _, ok := registry.Get("file_write"); !ok {
		t.Fatal("phase selection removed an executable tool from the shared registry")
	}
	selected := agent.ToolDefinitions(ctx, registry.List())
	selected[0].Description = "request-local"
	selected[0].Parameters["type"] = "changed"
	registered, _ := registry.Get(selected[0].Name)
	if registered.Description == "request-local" || registered.Parameters["type"] == "changed" {
		t.Fatal("request-local tool selection mutated a shared registry definition")
	}
}

func TestSelectPhaseToolsIsDeterministicAndBounded(t *testing.T) {
	definitions := make([]*tool.Definition, 0, maxToolsPerPhase+2)
	for i := maxToolsPerPhase + 1; i >= 0; i-- {
		definitions = append(definitions, &tool.Definition{Name: "graph_tool_" + string(rune('a'+i))})
	}

	got := selectPhaseTools(ToolPhaseDiscover, definitions)
	if len(got) != maxToolsPerPhase {
		t.Fatalf("selected tools = %d, want cap %d", len(got), maxToolsPerPhase)
	}
	if got[0].Name != "graph_tool_a" || got[len(got)-1].Name != "graph_tool_h" {
		t.Fatalf("selected names = %v, want sorted first %d tools", toolNames(got), maxToolsPerPhase)
	}
}

func TestToolPhaseMatchesOnlyRelevantTools(t *testing.T) {
	tests := []struct {
		phase ToolPhase
		name  string
		want  bool
	}{
		{ToolPhasePlan, "file_read", true},
		{ToolPhasePlan, "task_status", true},
		{ToolPhasePlan, "file_write", false},
		{ToolPhaseImplement, "file_write", true},
		{ToolPhaseImplement, "shell", true},
		{ToolPhaseVerify, "run_tests", true},
		{ToolPhaseVerify, "file_write", false},
		{ToolPhase("unknown"), "file_read", false},
	}
	for _, test := range tests {
		if got := toolMatchesPhase(test.phase, test.name); got != test.want {
			t.Errorf("toolMatchesPhase(%q, %q) = %v, want %v", test.phase, test.name, got, test.want)
		}
	}
}

func toolNames(definitions []*tool.Definition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	sort.Strings(names)
	return names
}
