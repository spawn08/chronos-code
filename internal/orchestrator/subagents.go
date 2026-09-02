package orchestrator

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/harness"
)

const configuredSubagentTimeout = 10 * time.Minute

type subagentTurnKey struct{}
type subagentPathKey struct{}

type subagentTurnState struct {
	modelCalls atomic.Int32
	maxCalls   int32
}

func withSubagentTurnState(ctx context.Context, maxModelCalls int, rootAgent string) context.Context {
	if ctx.Value(subagentTurnKey{}) != nil {
		return ctx
	}
	ctx = context.WithValue(ctx, subagentTurnKey{}, &subagentTurnState{maxCalls: int32(maxModelCalls)})
	if rootAgent != "" {
		ctx = context.WithValue(ctx, subagentPathKey{}, []string{rootAgent})
	}
	return ctx
}

func claimTurnModelCall(ctx context.Context) error {
	state, _ := ctx.Value(subagentTurnKey{}).(*subagentTurnState)
	if state == nil || state.maxCalls <= 0 {
		return nil
	}
	if calls := state.modelCalls.Add(1); calls > state.maxCalls {
		return fmt.Errorf("model call limit exceeded: maximum %d per turn", state.maxCalls)
	}
	return nil
}

type configuredAgentRunner struct {
	agents   map[string]*agent.Agent
	fallback harness.Runner
}

func (r configuredAgentRunner) Run(ctx context.Context, spec harness.SubAgentSpec, task string) (string, error) {
	path, _ := ctx.Value(subagentPathKey{}).([]string)
	for _, agentID := range path {
		if agentID == spec.Name {
			return "", fmt.Errorf("subagent delegation cycle detected: %v -> %s", path, spec.Name)
		}
	}
	nextPath := append(append([]string(nil), path...), spec.Name)
	ctx = context.WithValue(ctx, subagentPathKey{}, nextPath)
	if configured := r.agents[spec.Name]; configured != nil {
		runCtx, cancel := context.WithTimeout(ctx, configuredSubagentTimeout)
		defer cancel()
		result, err := configured.Execute(runCtx, task)
		if err != nil {
			return "", fmt.Errorf("configured subagent %q: %w", spec.Name, err)
		}
		return result, nil
	}
	return r.fallback.Run(ctx, spec, task)
}

// setupSubAgents makes every configured peer available as a real delegation
// target while retaining dynamic subagents through the standard harness runner.
func setupSubAgents(agents map[string]*agent.Agent) error {
	for parentID, parent := range agents {
		svc, err := harness.NewSubAgentService(parent)
		if err != nil {
			return fmt.Errorf("configure subagents for %q: %w", parentID, err)
		}
		for childID, child := range agents {
			if childID == parentID {
				continue
			}
			if err := svc.Register(harness.SubAgentSpec{
				Name:        childID,
				Description: child.Description,
			}); err != nil {
				return fmt.Errorf("register subagent %q for %q: %w", childID, parentID, err)
			}
		}
		harness.Attach(svc, configuredAgentRunner{
			agents:   agents,
			fallback: harness.NewInProcessRunner(svc),
		})
	}
	return nil
}
