package orchestrator

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos/sdk/agent"
)

// maxTurnsController caps the tool loop at limit model turns. A turn that
// requests tools is counted when its tool round completes, so a Stop never
// leaves a tool call without its result. It wraps any controller already
// installed (the renewable-window governor) so both policies apply.
type maxTurnsController struct {
	limit int
	inner agent.ToolLoopController
	hit   atomic.Bool
}

// AfterToolRound implements agent.ToolLoopController.
func (c *maxTurnsController) AfterToolRound(ctx context.Context, round agent.ToolRound) (agent.ToolLoopAction, error) {
	if c.inner != nil {
		action, err := c.inner.AfterToolRound(ctx, round)
		if err != nil || action.Stop {
			return action, err
		}
	}
	if round.Iteration >= c.limit {
		c.hit.Store(true)
		return agent.ToolLoopAction{Stop: true, Message: fmt.Sprintf("Stopped after reaching the maximum of %d model turns.", c.limit)}, nil
	}
	return agent.ToolLoopAction{}, nil
}

type maxTurnsKey struct{}

// withMaxTurns installs the loop policy for agentID: the turn cap (limit > 0)
// composed over base, or base alone when there is no cap.
func withMaxTurns(ctx context.Context, agentID string, limit int, base agent.ToolLoopController) context.Context {
	if limit <= 0 {
		return agent.WithToolLoopController(ctx, agentID, base)
	}
	controller := &maxTurnsController{limit: limit, inner: base}
	ctx = context.WithValue(ctx, maxTurnsKey{}, controller)
	return agent.WithToolLoopController(ctx, agentID, controller)
}

// pausedStopReason names why a paused tool loop stopped: the caller's turn
// cap, or (otherwise) the renewable-window no-progress governor.
func pausedStopReason(ctx context.Context) execution.StopReason {
	if c, ok := ctx.Value(maxTurnsKey{}).(*maxTurnsController); ok && c.hit.Load() {
		return execution.StopMaxTurns
	}
	return execution.StopNoProgress
}
