// Package teambuilder assembles chronos Team instances from YAML TeamConfig
// definitions (PRD P4-001). It replicates the strategy-specific wiring logic
// from chronos's own CLI (assembleTeamFromConfig) so that chronos-code can
// build teams purely from YAML without Go code.
package teambuilder

import (
	"context"
	"fmt"
	"strings"

	"github.com/spawn08/chronos/engine/graph"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/team"
)

// Build turns a TeamConfig plus a set of pre-built agents into a runnable
// Team. It handles strategy-specific wiring: graph compilation for
// swarm/hierarchy, coordinator/router/error-strategy knobs for the plain
// strategies.
func Build(tc *agent.TeamConfig, agents map[string]*agent.Agent) (*team.Team, error) {
	strategy, err := parseStrategy(tc.Strategy)
	if err != nil {
		return nil, fmt.Errorf("team %q: %w", tc.ID, err)
	}

	members := make([]*agent.Agent, 0, len(tc.Agents))
	for _, id := range tc.Agents {
		a, ok := agents[id]
		if !ok {
			return nil, fmt.Errorf("team %q references unknown agent %q", tc.ID, id)
		}
		members = append(members, a)
	}

	switch strategy {
	case team.StrategySwarm:
		return buildSwarm(tc, members)
	case team.StrategyHierarchy:
		return buildHierarchy(tc, agents, members)
	}

	t := team.New(tc.ID, tc.Name, strategy)
	for _, a := range members {
		t.AddAgent(a)
	}

	if tc.Coordinator != "" {
		coord, ok := agents[tc.Coordinator]
		if !ok {
			return nil, fmt.Errorf("team %q references unknown coordinator %q", tc.ID, tc.Coordinator)
		}
		t.SetCoordinator(coord)
	}
	if tc.MaxConcurrency > 0 {
		t.SetMaxConcurrency(tc.MaxConcurrency)
	}
	if tc.MaxIterations > 0 {
		t.SetMaxIterations(tc.MaxIterations)
	}
	if tc.ErrorStrategy != "" {
		es, esErr := parseErrorStrategy(tc.ErrorStrategy)
		if esErr != nil {
			return nil, esErr
		}
		t.SetErrorStrategy(es)
	}

	if strategy == team.StrategyRouter {
		if err := wireRouter(t, tc, members); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// BuildAll builds all teams from config, using the given pre-built agents.
func BuildAll(configs []agent.TeamConfig, agents map[string]*agent.Agent) (map[string]*team.Team, error) {
	teams := make(map[string]*team.Team, len(configs))
	for i := range configs {
		t, err := Build(&configs[i], agents)
		if err != nil {
			return nil, err
		}
		teams[t.ID] = t
	}
	return teams, nil
}

// Run executes a team with a user message, converting it to the graph.State
// format teams expect and extracting the response text from the result.
func Run(ctx context.Context, t *team.Team, message string) (string, error) {
	input := graph.State{"messages": message}
	result, err := t.Run(ctx, input)
	if err != nil {
		return "", fmt.Errorf("team %q: %w", t.ID, err)
	}
	if msg, ok := result["messages"].(string); ok {
		return msg, nil
	}
	if resp, ok := result["response"].(string); ok {
		return resp, nil
	}
	return fmt.Sprintf("%v", result), nil
}

func buildSwarm(tc *agent.TeamConfig, members []*agent.Agent) (*team.Team, error) {
	t, err := team.NewSwarm(team.SwarmConfig{
		Agents:       members,
		InitialAgent: tc.InitialAgent,
		MaxHandoffs:  tc.MaxHandoffs,
	})
	if err != nil {
		return nil, fmt.Errorf("team %q: %w", tc.ID, err)
	}
	t.ID = tc.ID
	t.Name = tc.Name
	return t, nil
}

func buildHierarchy(tc *agent.TeamConfig, agents map[string]*agent.Agent, members []*agent.Agent) (*team.Team, error) {
	if tc.Coordinator == "" {
		return nil, fmt.Errorf("team %q: hierarchy strategy requires a coordinator", tc.ID)
	}
	root, ok := agents[tc.Coordinator]
	if !ok {
		return nil, fmt.Errorf("team %q references unknown coordinator %q", tc.ID, tc.Coordinator)
	}
	workers := make([]*agent.Agent, 0, len(members))
	for _, a := range members {
		if a.ID == tc.Coordinator {
			continue
		}
		workers = append(workers, a)
	}
	t, err := team.NewHierarchy(team.HierarchyConfig{
		Root: &team.SupervisorNode{Supervisor: root, Workers: workers},
	})
	if err != nil {
		return nil, fmt.Errorf("team %q: %w", tc.ID, err)
	}
	t.ID = tc.ID
	t.Name = tc.Name
	return t, nil
}

func wireRouter(t *team.Team, tc *agent.TeamConfig, members []*agent.Agent) error {
	mode := strings.ToLower(strings.TrimSpace(tc.Router))
	if mode == "" {
		mode = "model"
	}
	switch mode {
	case "model":
		provider, err := resolveRouterProvider(tc, members)
		if err != nil {
			return err
		}
		if provider == nil {
			return fmt.Errorf("team %q: router strategy (model) requires a router_model or at least one member agent with a model", tc.ID)
		}
		t.SetModelRouter(team.NewModelRouter(provider))
		return nil
	case "capability":
		return nil
	default:
		return fmt.Errorf("team %q: unknown router mode %q", tc.ID, mode)
	}
}

func resolveRouterProvider(tc *agent.TeamConfig, members []*agent.Agent) (model.Provider, error) {
	if tc.RouterModel.Provider != "" {
		provider, err := agent.BuildProvider(tc.RouterModel)
		if err != nil {
			return nil, fmt.Errorf("team %q: router_model: %w", tc.ID, err)
		}
		return provider, nil
	}
	for _, a := range members {
		if a.Model != nil {
			return a.Model, nil
		}
	}
	return nil, nil
}

func parseStrategy(s string) (team.Strategy, error) {
	switch strings.ToLower(s) {
	case "sequential":
		return team.StrategySequential, nil
	case "parallel":
		return team.StrategyParallel, nil
	case "router":
		return team.StrategyRouter, nil
	case "coordinator":
		return team.StrategyCoordinator, nil
	case "swarm":
		return team.StrategySwarm, nil
	case "hierarchy":
		return team.StrategyHierarchy, nil
	default:
		return "", fmt.Errorf("unknown strategy %q", s)
	}
}

func parseErrorStrategy(s string) (team.ErrorStrategy, error) {
	switch strings.ToLower(s) {
	case "fail_fast", "failfast":
		return team.ErrorStrategyFailFast, nil
	case "collect":
		return team.ErrorStrategyCollect, nil
	case "best_effort", "besteffort":
		return team.ErrorStrategyBestEffort, nil
	default:
		return 0, fmt.Errorf("unknown error strategy %q", s)
	}
}
