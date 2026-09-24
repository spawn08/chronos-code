package security

import (
	"context"
	"fmt"
	"strings"
)

// Effect identifies an authority-bearing operation independently of tool names.
type Effect string

const (
	EffectRead             Effect = "read"
	EffectScratchWrite     Effect = "scratch_write"
	EffectDeliveryWrite    Effect = "delivery_write"
	EffectProcessExecution Effect = "process_execution"
	EffectNetwork          Effect = "network"
	EffectExternalMutation Effect = "external_mutation"
)

type effectGrantKey struct{}

// WithEffectGrant binds the exact effects authorized by the host for a run.
// An absent grant preserves execution-v1 behavior; an explicit empty grant is
// read-nothing and effect-nothing.
func WithEffectGrant(ctx context.Context, effects ...Effect) context.Context {
	grant := make(map[Effect]struct{}, len(effects))
	for _, effect := range effects {
		grant[effect] = struct{}{}
	}
	return context.WithValue(ctx, effectGrantKey{}, grant)
}

// EffectGrantFromContext returns a detached copy of the current host grant.
func EffectGrantFromContext(ctx context.Context) (map[Effect]struct{}, bool) {
	if ctx == nil {
		return nil, false
	}
	stored, ok := ctx.Value(effectGrantKey{}).(map[Effect]struct{})
	if !ok {
		return nil, false
	}
	grant := make(map[Effect]struct{}, len(stored))
	for effect := range stored {
		grant[effect] = struct{}{}
	}
	return grant, true
}

func requireToolEffects(ctx context.Context, toolName string) error {
	grant, constrained := EffectGrantFromContext(ctx)
	if !constrained {
		return nil
	}
	for _, effect := range toolEffects(toolName) {
		if _, ok := grant[effect]; !ok {
			return fmt.Errorf("security: tool %q requires undelegated effect %q", toolName, effect)
		}
	}
	return nil
}

func toolEffects(name string) []Effect {
	switch name {
	case "file_read", "file_list", "file_glob", "file_grep", "workspace_info",
		"codebase_map", "codebase_search", "graph_query", "find_callers",
		"find_implementations", "impact_analysis", "test_map", "co_change",
		"multi_resolution_view", "resolve_symbol", "read_stored_result":
		return []Effect{EffectRead}
	case "file_write", "apply_patch":
		return []Effect{EffectDeliveryWrite}
	case "shell", "shell_auto":
		return []Effect{EffectProcessExecution}
	default:
		if strings.HasPrefix(name, "mcp__") {
			return []Effect{EffectNetwork, EffectExternalMutation}
		}
		return []Effect{EffectExternalMutation}
	}
}
