package orchestrator

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

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
