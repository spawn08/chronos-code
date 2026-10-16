package orchestrator

import (
	"context"
	"fmt"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/storage"
)

// DisableTools applies a run-scoped evaluator ablation after startup without
// bypassing the normal registry's permissions or changing registered schemas.
func (o *Orchestrator) DisableTools(names ...string) {
	for _, a := range o.agents {
		for _, name := range names {
			if def, ok := a.Tools.Get(name); ok {
				copy := *def
				copy.Permission = tool.PermDeny
				a.Tools.Register(&copy)
			}
		}
	}
}

func (o *Orchestrator) SessionTraces(ctx context.Context, sessionID string) ([]*storage.Trace, error) {
	if o == nil || o.store == nil {
		return nil, fmt.Errorf("session trace storage unavailable")
	}
	return o.store.ListTraces(ctx, sessionID)
}
