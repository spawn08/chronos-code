package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/router"
)

const (
	capabilityGraphCode      = "graph:code"
	capabilityLSPTools       = "lsp:tools"
	capabilityPlanMode       = "planning:plan-mode"
	capabilityClosedLoopPPD  = "planning:closed-loop-ppd"
	capabilityToolPrefix     = "tool:"
	capabilityMCPPrefix      = "mcp:"
	capabilityWriteFiles     = "write:files"
	capabilityWriteShell     = "write:shell"
	runtimeMCPToolNamePrefix = "mcp__"
)

// RuntimeCapabilityManifest is a snapshot of capabilities proven by the
// completed startup path. Agent-scoped entries are never satisfied by a tool
// installed on a different agent.
type RuntimeCapabilityManifest struct {
	Capabilities []config.Capability
}

func buildRuntimeCapabilityManifest(agents map[string]*agent.Agent, graphAvailable bool) RuntimeCapabilityManifest {
	manifest := RuntimeCapabilityManifest{}
	add := func(name, agentID string) {
		manifest.Capabilities = append(manifest.Capabilities, config.Capability{Name: name, Agent: agentID})
	}
	add(capabilityPlanMode, "")
	if graphAvailable {
		add(capabilityGraphCode, "")
	}

	agentIDs := make([]string, 0, len(agents))
	for agentID := range agents {
		agentIDs = append(agentIDs, agentID)
	}
	sort.Strings(agentIDs)
	for _, agentID := range agentIDs {
		a := agents[agentID]
		if a == nil || a.Tools == nil {
			continue
		}
		seenMCP := make(map[string]struct{})
		for _, definition := range a.Tools.List() {
			if definition == nil || definition.Handler == nil || definition.Permission == tool.PermDeny {
				continue
			}
			add(capabilityToolPrefix+definition.Name, agentID)
			switch definition.Name {
			case "file_write":
				add(capabilityWriteFiles, agentID)
			case "shell", "shell_auto":
				add(capabilityWriteShell, agentID)
			}
			if strings.HasPrefix(definition.Name, "lsp_") {
				add(capabilityLSPTools, "")
			}
			if namespace := mcpNamespace(definition.Name); namespace != "" {
				if _, ok := seenMCP[namespace]; !ok {
					add(capabilityMCPPrefix+namespace, agentID)
					seenMCP[namespace] = struct{}{}
				}
			}
		}
	}

	sort.Slice(manifest.Capabilities, func(i, j int) bool {
		left, right := manifest.Capabilities[i], manifest.Capabilities[j]
		if left.Agent != right.Agent {
			return left.Agent < right.Agent
		}
		return left.Name < right.Name
	})
	manifest.Capabilities = compactCapabilities(manifest.Capabilities)
	return manifest
}

func mcpNamespace(toolName string) string {
	if !strings.HasPrefix(toolName, runtimeMCPToolNamePrefix) {
		return ""
	}
	parts := strings.SplitN(strings.TrimPrefix(toolName, runtimeMCPToolNamePrefix), "__", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[0]
}

func compactCapabilities(capabilities []config.Capability) []config.Capability {
	if len(capabilities) < 2 {
		return capabilities
	}
	compacted := capabilities[:1]
	for _, capability := range capabilities[1:] {
		last := compacted[len(compacted)-1]
		if capability.Agent != last.Agent || capability.Name != last.Name {
			compacted = append(compacted, capability)
		}
	}
	return compacted
}

func validateRuntimeCapabilities(cfg *config.Config, agents map[string]*agent.Agent, graphAvailable bool, routingConfig *router.Config) (RuntimeCapabilityManifest, []string, error) {
	manifest := buildRuntimeCapabilityManifest(agents, graphAvailable)
	available := config.CapabilityManifest{Capabilities: manifest.Capabilities}
	required := make([]config.Capability, 0, len(cfg.RuntimeCaps.Capabilities))
	for _, agentConfig := range cfg.Agents {
		for _, configuredTool := range agentConfig.Tools {
			if configuredTool.Name != "" {
				if !isConfiguredToolSupported(configuredTool.Name) {
					if configuredTool.Name == "semantic_search" {
						return manifest, nil, fmt.Errorf("validate runtime capabilities: required capability %q is not available for agent %q; remove the legacy semantic_search entry from its agent YAML (use codebase_search for indexed code search when the graph is enabled)", capabilityToolPrefix+configuredTool.Name, agentConfig.ID)
					}
					return manifest, nil, fmt.Errorf("validate runtime capabilities: required capability %q is not available for agent %q", capabilityToolPrefix+configuredTool.Name, agentConfig.ID)
				}
				required = append(required, config.Capability{Name: capabilityToolPrefix + configuredTool.Name, Agent: agentConfig.ID})
			}
		}
	}
	required = append(required, cfg.RuntimeCaps.Capabilities...)
	if routingConfig != nil && routingConfig.PPD.Mode == router.PPDModeEnabled {
		required = append(required, config.Capability{Name: capabilityClosedLoopPPD})
	}

	warnings := make([]string, 0)
	for _, requirement := range required {
		probe := requirement
		probe.Optional = false
		err := (config.CapabilityManifest{Capabilities: []config.Capability{probe}}).Validate(available)
		if err == nil {
			continue
		}
		if requirement.Optional {
			warnings = append(warnings, strings.Replace(err.Error(), "required capability", "optional capability", 1))
			continue
		}
		return manifest, warnings, fmt.Errorf("validate runtime capabilities: %w", err)
	}
	sort.Strings(warnings)
	return manifest, warnings, nil
}

func isConfiguredToolSupported(name string) bool {
	switch name {
	case "shell", "shell_auto", "file_read", "file_write", "file_list", "file_glob", "file_grep":
		return true
	default:
		return false
	}
}

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
