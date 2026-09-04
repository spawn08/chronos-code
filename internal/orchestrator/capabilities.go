package orchestrator

import (
	"context"
	"sort"
	"strings"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

// ToolPhase limits the tool schemas advertised for one bounded unit of work.
// The underlying registry remains the authority for execution and is never
// changed as a result of selecting a phase.
type ToolPhase string

const (
	ToolPhaseDiscover  ToolPhase = "discover"
	ToolPhasePlan      ToolPhase = "plan"
	ToolPhaseImplement ToolPhase = "implement"
	ToolPhaseVerify    ToolPhase = "verify"

	maxToolsPerPhase = 8
)

// WithToolPhase selects the schemas relevant to phase for this request only.
// It deliberately stores a copied selection in ctx rather than registering or
// unregistering tools, so concurrent turns retain their own complete registry.
func WithToolPhase(ctx context.Context, phase ToolPhase, registry *tool.Registry) context.Context {
	if registry == nil {
		return ctx
	}
	return agent.WithToolDefinitions(ctx, selectPhaseTools(phase, registry.List()))
}

func selectPhaseTools(phase ToolPhase, definitions []*tool.Definition) []*tool.Definition {
	selected := make([]*tool.Definition, 0, maxToolsPerPhase)
	for _, definition := range definitions {
		if definition != nil && toolMatchesPhase(phase, definition.Name) {
			selected = append(selected, definition)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
	if len(selected) > maxToolsPerPhase {
		selected = selected[:maxToolsPerPhase]
	}
	return selected
}

func toolMatchesPhase(phase ToolPhase, name string) bool {
	name = strings.ToLower(name)
	readOnly := strings.HasPrefix(name, "file_read") || strings.HasPrefix(name, "file_glob") ||
		strings.HasPrefix(name, "file_grep") || strings.HasPrefix(name, "graph_") ||
		strings.HasPrefix(name, "lsp_") || name == "workspace_info"

	switch phase {
	case ToolPhaseDiscover:
		return readOnly
	case ToolPhasePlan:
		return readOnly || strings.Contains(name, "plan") || strings.Contains(name, "task")
	case ToolPhaseImplement:
		return readOnly || strings.HasPrefix(name, "file_write") || strings.HasPrefix(name, "file_edit") ||
			name == "shell" || name == "apply_patch"
	case ToolPhaseVerify:
		return readOnly || name == "shell" || strings.Contains(name, "test") || strings.Contains(name, "diagnostic")
	default:
		return false
	}
}
