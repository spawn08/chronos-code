package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spawn08/chronos/engine/model"
)

type storedAgentReply struct {
	Response   model.ChatResponse `json:"response"`
	UsageKnown bool               `json:"usage_known"`
}

// CheckpointAgentReply records a validated terminal reply only after every
// provider call in this invocation has reconciled usage. Earlier tool calls
// must also have complete round checkpoints; a missing receipt remains parked.
func (s *DeliveryStore) CheckpointAgentReply(ctx context.Context, lease Lease, invocation, role, node, input, modelID string, response *model.ChatResponse) error {
	if invocation == "" || role == "" || modelID == "" || response == nil || response.Err != nil ||
		response.StopReason != model.StopReasonEnd || len(response.ToolCalls) != 0 {
		return ErrInvalidDelivery
	}
	data, err := json.Marshal(storedAgentReply{Response: *response, UsageKnown: response.UsageKnown})
	if err != nil || len(data) > 1<<20 {
		return errors.Join(err, ErrInvalidDelivery)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent reply checkpoint: %w", err)
	}
	defer tx.Rollback()
	if err := verifyLease(ctx, tx, lease, s.clock.Now().UTC()); err != nil {
		return err
	}
	var rounds, billed, reconciled int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_tool_rounds WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND invocation_id = ? AND goal_revision = ?`,
		lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, invocation, lease.Delivery.CurrentGoalRevision).Scan(&rounds); err != nil {
		return fmt.Errorf("count preceding tool rounds: %w", err)
	}
	prefix := invocation + ":"
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN status = 'reconciled' AND model = ? THEN 1 ELSE 0 END), 0) FROM delivery_usage_calls WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND node_id = ? AND substr(call_id, 1, ?) = ?`,
		modelID, lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, node, len(prefix), prefix).Scan(&billed, &reconciled); err != nil {
		return fmt.Errorf("count agent provider calls: %w", err)
	}
	if billed != rounds+1 || reconciled != billed {
		return ErrUsageOutcomeUnknown
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_agent_replies (tenant_id, repository_id, delivery_id, invocation_id, attempt, role_id, node_id, goal_revision, input_fingerprint, model_id, response_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, invocation, lease.Attempt, role, node, lease.Delivery.CurrentGoalRevision, roundInputFingerprint(input), modelID, string(data)); err != nil {
		return fmt.Errorf("persist agent reply checkpoint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent reply checkpoint: %w", err)
	}
	return nil
}

func (s *DeliveryStore) ResumeAgentReply(ctx context.Context, lease Lease, role, node, input, modelID string) (*model.ChatResponse, error) {
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
	err = tx.QueryRowContext(ctx, `SELECT input_fingerprint, model_id, goal_revision, response_json FROM delivery_agent_replies WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND role_id = ? AND node_id = ? AND attempt < ? ORDER BY attempt DESC LIMIT 1`,
		lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, role, node, lease.Attempt).Scan(&fingerprint, &savedModel, &revision, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load agent reply checkpoint: %w", err)
	}
	if fingerprint != roundInputFingerprint(input) || savedModel != modelID || revision != lease.Delivery.CurrentGoalRevision {
		return nil, ErrEffectNeedsReconciliation
	}
	var saved storedAgentReply
	if err := json.Unmarshal([]byte(data), &saved); err != nil || saved.Response.StopReason != model.StopReasonEnd || len(saved.Response.ToolCalls) != 0 || saved.Response.Err != nil {
		return nil, errors.Join(err, ErrEffectNeedsReconciliation)
	}
	response := saved.Response
	response.UsageKnown = saved.UsageKnown
	return &response, nil
}
