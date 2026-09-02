package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/harness"
)

const configuredSubagentTimeout = 10 * time.Minute

type configuredAgentRunner struct {
	agents   map[string]*agent.Agent
	fallback harness.Runner
}

func (r configuredAgentRunner) Run(ctx context.Context, spec harness.SubAgentSpec, task string) (string, error) {
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
