package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
)

type ToolRoundRecord struct {
	InvocationID   string
	RoleID         string
	NodeID         string
	Input          string
	Model          string
	Number         int
	Attempt        int64
	Messages       []model.Message
	EffectfulCalls []string
}

func roundInputFingerprint(input string) string {
	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])
}

// CheckpointToolRound commits only a complete call/result exchange under the
// active worker lease. It cannot bless an unobserved effect.
func (s *DeliveryStore) CheckpointToolRound(ctx context.Context, lease Lease, round ToolRoundRecord) error {
	if round.InvocationID == "" || round.RoleID == "" || round.Model == "" || round.Number < 1 || !completeToolRound(round.Messages) {
		return ErrInvalidDelivery
	}
	data, err := agent.EncodeToolRound(round.Messages)
	if err != nil || len(data) > 4<<20 {
		return errors.Join(err, ErrInvalidDelivery)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tool round checkpoint: %w", err)
	}
	defer tx.Rollback()
	if err := verifyLease(ctx, tx, lease, s.clock.Now().UTC()); err != nil {
		return err
	}
	effectful := make(map[string]bool, len(round.EffectfulCalls))
	for _, id := range round.EffectfulCalls {
		effectful[id] = true
	}
	for i := len(round.Messages) - 1; i >= 0; i-- {
		msg := round.Messages[i]
		if msg.Role == model.RoleAssistant && len(msg.ToolCalls) > 0 {
			for j, call := range msg.ToolCalls {
				op, err := loadOperation(ctx, tx, lease, round.InvocationID+":"+call.ID)
				if errors.Is(err, ErrOperationNotFound) {
					if effectful[call.ID] {
						return ErrEffectNeedsReconciliation
					}
					continue // read-only calls have no effect record
				}
				if err != nil {
					return err
				}
				if (op.Status != OperationObserved && op.Status != OperationReconciled) || string(op.Result) != round.Messages[i+j+1].Content {
					return ErrEffectNeedsReconciliation
				}
			}
			break // earlier rounds have their own checkpoints and invocation IDs
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO delivery_tool_rounds (tenant_id, repository_id, delivery_id, invocation_id, round_number, attempt, role_id, node_id, goal_revision, input_fingerprint, model_id, messages_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, round.InvocationID, round.Number, lease.Attempt, round.RoleID, round.NodeID, lease.Delivery.CurrentGoalRevision, roundInputFingerprint(round.Input), round.Model, string(data))
	if err != nil {
		return fmt.Errorf("persist tool round checkpoint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tool round checkpoint: %w", err)
	}
	return nil
}

func completeToolRound(messages []model.Message) bool {
	if len(messages) < 3 || messages[len(messages)-1].Role != model.RoleTool {
		return false
	}
	seen := make(map[string]bool)
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		if msg.Role == model.RoleTool {
			return false
		}
		if msg.Role != model.RoleAssistant || len(msg.ToolCalls) == 0 {
			continue
		}
		if i+len(msg.ToolCalls) >= len(messages) {
			return false
		}
		for j, call := range msg.ToolCalls {
			result := messages[i+j+1]
			if call.ID == "" || seen[call.ID] || result.Role != model.RoleTool || result.ToolCallID != call.ID || result.Name != call.Name {
				return false
			}
			seen[call.ID] = true
		}
		i += len(msg.ToolCalls)
	}
	return len(seen) > 0
}

// ResumeToolRound returns the last complete round for the same role and node.
// A changed input/model/goal never inherits another run's effects.
func (s *DeliveryStore) ResumeToolRound(ctx context.Context, lease Lease, role, node, input, modelID string) ([]model.Message, error) {
	if role == "" || modelID == "" {
		return nil, ErrInvalidDelivery
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := verifyLease(ctx, tx, lease, s.clock.Now().UTC()); err != nil {
		return nil, err
	}
	var fingerprint, savedModel, data string
	var revision GoalRevision
	err = tx.QueryRowContext(ctx, `SELECT input_fingerprint, model_id, goal_revision, messages_json FROM delivery_tool_rounds WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND role_id = ? AND node_id = ? AND attempt < ? ORDER BY attempt DESC, round_number DESC LIMIT 1`,
		lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, role, node, lease.Attempt).Scan(&fingerprint, &savedModel, &revision, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load tool round checkpoint: %w", err)
	}
	if fingerprint != roundInputFingerprint(input) || savedModel != modelID || revision != lease.Delivery.CurrentGoalRevision {
		return nil, ErrEffectNeedsReconciliation
	}
	messages, err := agent.DecodeToolRound([]byte(data))
	if err != nil || !completeToolRound(messages) {
		return nil, errors.Join(err, ErrEffectNeedsReconciliation)
	}
	return messages, nil
}

// CoversOperation requires both the durable effect receipt and its matching
// assistant call/tool result pair before a replacement may continue.
func (s *DeliveryStore) CoversOperation(ctx context.Context, lease Lease, op Operation) (bool, error) {
	if op.Status != OperationObserved && op.Status != OperationReconciled {
		return false, nil
	}
	var data, invocation string
	err := s.db.QueryRowContext(ctx, `SELECT invocation_id, messages_json FROM delivery_tool_rounds WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND attempt = ? AND role_id = ? AND node_id = ? ORDER BY round_number DESC LIMIT 1`,
		lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, op.Attempt, op.RoleID, op.NodeID).Scan(&invocation, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	messages, err := agent.DecodeToolRound([]byte(data))
	if err != nil || !completeToolRound(messages) {
		return false, errors.Join(err, ErrEffectNeedsReconciliation)
	}
	for i, msg := range messages {
		for j, call := range msg.ToolCalls {
			if msg.Role == model.RoleAssistant && op.ID == invocation+":"+call.ID && op.Kind == call.Name && string(op.Result) == messages[i+j+1].Content {
				return true, nil
			}
		}
	}
	return false, nil
}

// CheckpointedCallCount counts covered tool-call and terminal model replies.
// A billed reply without either checkpoint cannot be replayed.
func (s *DeliveryStore) CheckpointedCallCount(ctx context.Context, lease Lease) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT
      (SELECT COUNT(*) FROM delivery_tool_rounds WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND attempt < ? AND goal_revision = ?) +
      (SELECT COUNT(*) FROM delivery_agent_replies WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND attempt < ? AND goal_revision = ?)`,
		lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, lease.Attempt, lease.Delivery.CurrentGoalRevision,
		lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, lease.Attempt, lease.Delivery.CurrentGoalRevision).Scan(&count)
	return count, err
}

func (s *DeliveryStore) CheckpointedCallsByNode(ctx context.Context, lease Lease) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, COUNT(*) FROM (
      SELECT node_id FROM delivery_tool_rounds WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND attempt < ? AND goal_revision = ?
      UNION ALL
      SELECT node_id FROM delivery_agent_replies WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND attempt < ? AND goal_revision = ?
    ) GROUP BY node_id`,
		lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, lease.Attempt, lease.Delivery.CurrentGoalRevision,
		lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, lease.Attempt, lease.Delivery.CurrentGoalRevision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int64)
	for rows.Next() {
		var node string
		var count int64
		if err := rows.Scan(&node, &count); err != nil {
			return nil, err
		}
		counts[node] = count
	}
	return counts, rows.Err()
}
