package orchestrator

import (
	"context"
	"fmt"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

type deliveryToolRoundJournal struct {
	attempt *execution.Execution
	roles   map[string]*agent.Agent
}

func (j deliveryToolRoundJournal) ResumeToolRound(ctx context.Context, agentID, input, modelID string) ([]model.Message, error) {
	identity, ok := agent.RunIdentityFromContext(ctx)
	if !ok || identity.RoleID != agentID {
		return nil, execution.ErrInvalidDelivery
	}
	lease, ok := execution.OperationLeaseFromContext(ctx)
	if !ok {
		return nil, execution.ErrInvalidDelivery
	}
	return lease.Store.ResumeToolRound(ctx, j.attempt.Lease, identity.RoleID, identity.NodeID, input, modelID)
}

func (j deliveryToolRoundJournal) CheckpointToolRound(ctx context.Context, agentID, input, modelID string, iteration int, messages []model.Message) error {
	identity, ok := agent.RunIdentityFromContext(ctx)
	if !ok || identity.RoleID != agentID || identity.InvocationID == "" {
		return execution.ErrInvalidDelivery
	}
	lease, ok := execution.OperationLeaseFromContext(ctx)
	if !ok {
		return execution.ErrInvalidDelivery
	}
	role := j.roles[agentID]
	if role == nil || role.Tools == nil {
		return execution.ErrEffectNeedsReconciliation
	}
	var effectful []string
	// Resolve effects conservatively: a dynamic effect resolver requires a
	// receipt even when a particular invocation ultimately reads only.
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != model.RoleAssistant || len(msg.ToolCalls) == 0 {
			continue
		}
		for _, call := range msg.ToolCalls {
			definition, ok := role.Tools.Get(call.Name)
			if !ok {
				continue // the SDK recorded an unknown-tool error
			}
			if definition.ResolveEffects != nil {
				effectful = append(effectful, call.ID)
				continue
			}
			for _, effect := range definition.Effects {
				if effect != tool.EffectRead {
					effectful = append(effectful, call.ID)
					break
				}
			}
		}
		break
	}
	if err := lease.Store.CheckpointToolRound(ctx, j.attempt.Lease, execution.ToolRoundRecord{
		InvocationID: identity.InvocationID, RoleID: identity.RoleID, NodeID: identity.NodeID,
		Input: input, Model: modelID, Number: iteration, Messages: messages, EffectfulCalls: effectful,
	}); err != nil {
		return fmt.Errorf("persist delivery tool exchange: %w", err)
	}
	return nil
}

var _ agent.ToolRoundJournal = deliveryToolRoundJournal{}
