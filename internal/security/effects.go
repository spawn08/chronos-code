package security

import (
	"context"

	"github.com/spawn08/chronos/engine/tool"
)

// Effect identifies an authority-bearing operation independently of tool names.
type Effect = tool.Effect

const (
	EffectRead             = tool.EffectRead
	EffectScratchWrite     = tool.EffectScratchWrite
	EffectDeliveryWrite    = tool.EffectDeliveryWrite
	EffectProcessExecution = tool.EffectProcessExecution
	EffectNetwork          = tool.EffectNetwork
	EffectExternalMutation = tool.EffectExternalMutation
)

// WithEffectGrant binds the exact effects authorized by the host for a run.
// An absent grant preserves execution-v1 behavior; an explicit empty grant is
// read-nothing and effect-nothing.
func WithEffectGrant(ctx context.Context, effects ...Effect) context.Context {
	return tool.WithEffectGrant(ctx, effects...)
}

// EffectGrantFromContext returns a detached copy of the current host grant.
func EffectGrantFromContext(ctx context.Context) (map[Effect]struct{}, bool) {
	return tool.EffectGrantFromContext(ctx)
}
