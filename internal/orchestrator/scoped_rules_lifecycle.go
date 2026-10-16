package orchestrator

import (
	"context"

	"github.com/spawn08/chronos/sdk/agent"
)

func releaseScopedRuleState(ctx context.Context, a *agent.Agent) {
	if a == nil {
		return
	}
	for _, hook := range a.Hooks {
		if rules, ok := hook.(*scopedRulesHook); ok {
			rules.release(ctx)
		}
	}
}
