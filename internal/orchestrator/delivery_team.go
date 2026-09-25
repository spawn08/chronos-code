package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/teambuilder"
	"github.com/spawn08/chronos/engine/graph"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/team"
)

const maxTeamCheckpointBytes = 256 << 10

type teamCheckpoint struct {
	Version         int                    `json:"version"`
	TeamID          string                 `json:"team_id"`
	GoalRevision    execution.GoalRevision `json:"goal_revision"`
	Agents          []string               `json:"agents"`
	Responses       []string               `json:"responses"`
	ReconciledCalls int64                  `json:"reconciled_calls"`
}

// durableTeam constructs a request-local strategy so prior tenants' team bus
// and shared context cannot enter this worker attempt.
func (o *Orchestrator) durableTeam(id string) (*team.Team, error) {
	if o.cfg != nil {
		for i := range o.cfg.Teams {
			if o.cfg.Teams[i].ID == id {
				return teambuilder.Build(&o.cfg.Teams[i], o.agents)
			}
		}
	}
	source, ok := o.teams[id]
	if !ok || source == nil {
		return nil, fmt.Errorf("durable team %q is not configured", id)
	}
	if source.Strategy != team.StrategySequential {
		return nil, fmt.Errorf("durable team %q requires a checkpointed sequential strategy", id)
	}
	fresh := team.New(source.ID, source.Name, source.Strategy)
	for _, member := range source.Order {
		configured := o.agents[member]
		if configured == nil {
			return nil, fmt.Errorf("durable team %q has missing member %q", id, member)
		}
		fresh.AddAgent(configured)
	}
	return fresh, nil
}

// runDurableTeam resumes only a persisted prefix. Any provider call made
// after that prefix without a step checkpoint parks the work for evidence-led
// reconciliation instead of submitting the same agent step again.
func (o *Orchestrator) runDurableTeam(ctx context.Context, id, message string, attempt *execution.Execution) (string, error) {
	t, err := o.durableTeam(id)
	if err != nil {
		return "", err
	}
	if t.Strategy != team.StrategySequential || len(t.Order) == 0 {
		return "", fmt.Errorf("durable team %q requires nonempty sequential members", id)
	}
	for _, member := range t.Order {
		if role := o.agents[member]; role == nil || !hasDurableBudgetHook(role) {
			return "", fmt.Errorf("durable team member %q has no model-call accounting", member)
		}
	}
	usage, err := attempt.CumulativeUsage(ctx)
	if err != nil {
		return "", err
	}
	if usage.OutstandingCalls > 0 {
		return "", execution.ErrUsageOutcomeUnknown
	}
	attempts, err := attempt.PriorAttempts(ctx)
	if err != nil {
		return "", err
	}
	checkpoint := teamCheckpoint{Version: 1, TeamID: id, GoalRevision: attempt.Lease.Delivery.CurrentGoalRevision, Agents: append([]string(nil), t.Order...)}
	for _, prior := range attempts {
		if prior.Attempt >= attempt.Lease.Attempt || len(prior.Checkpoint) == 0 {
			continue
		}
		var loaded teamCheckpoint
		if err := json.Unmarshal(prior.Checkpoint, &loaded); err != nil {
			return "", fmt.Errorf("read prior team checkpoint: %w", err)
		}
		checkpoint = loaded
	}
	if checkpoint.Version != 1 || checkpoint.TeamID != id || checkpoint.GoalRevision != attempt.Lease.Delivery.CurrentGoalRevision ||
		!slices.Equal(checkpoint.Agents, t.Order) || len(checkpoint.Responses) > len(t.Order) || checkpoint.ReconciledCalls != usage.ReconciledCalls {
		return "", execution.ErrEffectNeedsReconciliation
	}
	ctx, err = o.executionTaskContext(ctx, "team:"+id)
	if err != nil {
		return "", err
	}
	result, err := t.RunSequentialWithCheckpoints(ctx, graph.State{"messages": message}, checkpoint.Responses, func(ctx context.Context, step int, member, response string) error {
		if step != len(checkpoint.Responses) || member != t.Order[step] || !utf8.ValidString(response) {
			return execution.ErrInvalidDelivery
		}
		current, err := attempt.CumulativeUsage(ctx)
		if err != nil {
			return err
		}
		if current.OutstandingCalls > 0 {
			return execution.ErrUsageOutcomeUnknown
		}
		checkpoint.Responses = append(checkpoint.Responses, response)
		checkpoint.ReconciledCalls = current.ReconciledCalls
		payload, err := json.Marshal(checkpoint)
		if err != nil {
			return fmt.Errorf("encode team step receipt: %w", err)
		}
		if len(payload) > maxTeamCheckpointBytes {
			return fmt.Errorf("team step receipt exceeds %d bytes", maxTeamCheckpointBytes)
		}
		return attempt.Checkpoint(ctx, payload)
	})
	if err != nil {
		return "", err
	}
	content, _ := result["response"].(string)
	return content, nil
}

func hasDurableBudgetHook(a *agent.Agent) bool {
	for _, hook := range a.Hooks {
		if _, ok := hook.(deliveryBudgetHook); ok {
			return true
		}
	}
	return false
}
