package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

const deferredSchemaLimit = 20

type deferredToolsHook struct {
	core []string
}

func (h deferredToolsHook) Before(_ context.Context, evt *hooks.Event) error {
	if evt == nil || evt.Type != hooks.EventModelCallBefore {
		return nil
	}
	req, ok := evt.Input.(*model.ChatRequest)
	if !ok || req == nil || len(req.Tools) == 0 {
		return nil
	}
	available := make(map[string]model.ToolDefinition, len(req.Tools))
	for _, def := range req.Tools {
		available[def.Function.Name] = def
	}
	tools := make([]model.ToolDefinition, 0, len(h.core))
	for _, name := range h.core {
		if def, exists := available[name]; exists {
			tools = append(tools, def)
		}
	}
	req.Tools = tools
	return nil
}

func (h deferredToolsHook) After(context.Context, *hooks.Event) error { return nil }

func (o *Orchestrator) installDeferredTools(a *agent.Agent) {
	if o.cfg == nil || !o.cfg.Tools.DeferredSchemas {
		return
	}
	core := map[string]bool{}
	var order []string
	for _, name := range append(append([]string(nil), o.cfg.Tools.CoreTools...), "discover_tools", "call_tool") {
		if name != "" && !core[name] {
			core[name] = true
			order = append(order, name)
		}
	}
	a.Tools.Register(&tool.Definition{
		Name: "discover_tools", Permission: tool.PermAllow, ParallelSafe: true, Effects: []tool.Effect{tool.EffectRead},
		Description: "Discover specialized schemas by namespace, exact name, or query. Returns at most 20 schemas; narrow a truncated query. Use call_tool to execute; discovery does not grant authority.",
		Parameters: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"namespace": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"}, "query": map[string]any{"type": "string"},
		}},
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			namespace, err := discoveryString(args, "namespace")
			if err != nil {
				return nil, err
			}
			name, err := discoveryString(args, "name")
			if err != nil {
				return nil, err
			}
			query, err := discoveryString(args, "query")
			if err != nil {
				return nil, err
			}
			definitions := a.Tools.List()
			if name != "" {
				def, exists := a.Tools.Get(name)
				if !exists {
					return nil, fmt.Errorf("unknown tool %q", name)
				}
				if core[name] {
					return nil, fmt.Errorf("tool %q is already a core tool", name)
				}
				if def.Permission == tool.PermDeny || def.Handler == nil {
					return nil, fmt.Errorf("tool %q unavailable", name)
				}
			}
			knownNamespace := namespace == ""
			matches := make([]map[string]any, 0)
			truncated := false
			for _, def := range definitions {
				if core[def.Name] {
					continue
				}
				if namespace != "" && namespace != toolNamespace(def.Name) && "mcp:"+namespace != toolNamespace(def.Name) {
					continue
				}
				knownNamespace = true
				if def.Permission == tool.PermDeny || def.Handler == nil || (name != "" && def.Name != name) {
					continue
				}
				if query != "" && !strings.Contains(strings.ToLower(def.Name+" "+def.Description), strings.ToLower(query)) {
					continue
				}
				if len(matches) == deferredSchemaLimit {
					truncated = true
					break
				}
				matches = append(matches, map[string]any{"name": def.Name, "namespace": toolNamespace(def.Name), "description": def.Description, "parameters": def.Parameters, "permission": def.Permission})
			}
			if !knownNamespace {
				return nil, fmt.Errorf("unknown tool namespace %q", namespace)
			}
			if name != "" && len(matches) == 0 {
				return nil, fmt.Errorf("tool %q does not match discovery filters", name)
			}
			return map[string]any{"tools": matches, "truncated": truncated}, nil
		},
	})
	a.Tools.Register(&tool.Definition{
		Name: "call_tool", Permission: tool.PermAllow,
		Description: "Execute a discovered tool with its normal authorization, approvals, hooks and output wrappers. Dispatch is ordered; never dispatch call_tool or discover_tools.",
		Parameters: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"name": map[string]any{"type": "string", "minLength": 1}, "arguments": map[string]any{"type": "object"},
		}, "required": []string{"name", "arguments"}},
		ResolveEffects: func(ctx context.Context, args map[string]any) ([]tool.Effect, error) {
			def, arguments, err := deferredTarget(a.Tools, args)
			if err != nil {
				return nil, err
			}
			if def.ResolveEffects != nil {
				return def.ResolveEffects(ctx, arguments)
			}
			return def.Effects, nil
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			def, arguments, err := deferredTarget(a.Tools, args)
			if err != nil {
				return nil, err
			}
			evt := &hooks.Event{Type: hooks.EventToolCallBefore, Name: def.Name, Input: arguments}
			// Re-enter hooks and the registry; raw handlers bypass Guard and approvals.
			if err := a.Hooks.Before(ctx, evt); err != nil {
				return nil, fmt.Errorf("specialized tool %q preflight: %w", def.Name, err)
			}
			result, err := a.Tools.Execute(ctx, def.Name, arguments)
			evt.Type, evt.Output, evt.Error = hooks.EventToolCallAfter, result, err
			if hookErr := a.Hooks.After(ctx, evt); hookErr != nil {
				return result, errors.Join(err, fmt.Errorf("specialized tool %q observation: %w", def.Name, hookErr))
			}
			return result, err
		},
	})
	a.Hooks = append(a.Hooks, deferredToolsHook{core: order})
	prev := a.ContextPinsFn
	a.ContextPinsFn = func(ctx context.Context) []model.Message {
		var pins []model.Message
		if prev != nil {
			pins = append(pins, prev(ctx)...)
		}
		return append(pins, model.Message{Role: model.RoleSystem, TurnScoped: true, Content: deferredCatalog(a.Tools, core)})
	}
}

func discoveryString(args map[string]any, key string) (string, error) {
	value, exists := args[key]
	if !exists {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("discover_tools: %s must be a string", key)
	}
	if len(text) > 256 {
		return "", fmt.Errorf("discover_tools: %s exceeds 256 bytes", key)
	}
	return strings.TrimSpace(text), nil
}

func deferredTarget(registry *tool.Registry, args map[string]any) (*tool.Definition, map[string]any, error) {
	name, ok := args["name"].(string)
	if !ok || name == "" || name == "call_tool" || name == "discover_tools" {
		return nil, nil, fmt.Errorf("invalid specialized tool name %q", name)
	}
	def, exists := registry.Get(name)
	if !exists {
		return nil, nil, fmt.Errorf("unknown tool %q", name)
	}
	if def.Permission == tool.PermDeny || def.Handler == nil {
		return nil, nil, fmt.Errorf("tool %q unavailable", name)
	}
	arguments, ok := args["arguments"].(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("arguments must be an object")
	}
	return def, arguments, nil
}

func deferredCatalog(registry *tool.Registry, core map[string]bool) string {
	type counts struct{ available, unavailable int }
	groups := map[string]counts{}
	for _, def := range registry.List() {
		if core[def.Name] {
			continue
		}
		namespace := toolNamespace(def.Name)
		group := groups[namespace]
		if def.Permission == tool.PermDeny || def.Handler == nil {
			group.unavailable++
		} else {
			group.available++
		}
		groups[namespace] = group
	}
	var order []string
	for namespace := range groups {
		order = append(order, namespace)
	}
	sort.Strings(order)
	var catalog strings.Builder
	catalog.WriteString("Specialized tool catalog (current registry; availability is not authorization). Use discover_tools with namespace/name/query, then call_tool with name and arguments. Approvals and effect grants still apply.")
	for i, namespace := range order {
		if i == 24 || catalog.Len() > 3000 {
			catalog.WriteString("\nAdditional namespaces omitted; discover_tools accepts a name or query.")
			break
		}
		group := groups[namespace]
		fmt.Fprintf(&catalog, "\n- %s: %d available, %d unavailable", validPrefix(namespace, 80), group.available, group.unavailable)
	}
	return catalog.String()
}

func toolNamespace(name string) string {
	if strings.HasPrefix(name, "mcp__") {
		parts := strings.SplitN(strings.TrimPrefix(name, "mcp__"), "__", 2)
		return "mcp:" + parts[0]
	}
	if strings.HasPrefix(name, "lsp_") {
		return "lsp"
	}
	if strings.HasPrefix(name, "memory_") {
		return "memory"
	}
	if strings.Contains(name, "subagent") {
		return "subagent"
	}
	switch name {
	case "codebase_search", "codebase_context", "resolve_symbol", "graph_query", "find_callers", "find_implementations", "impact_analysis", "test_map", "codebase_map", "multi_resolution_view":
		return "graph"
	}
	return "harness"
}
