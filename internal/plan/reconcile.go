package plan

import (
	"context"
	"errors"
	"fmt"
)

var ErrPlanReceiptNotEligible = errors.New("plan node receipt is not eligible for reconciliation")

// ReconcileAppliedReceipt commits a verified, applied patch after an expired
// owner was parked. The caller must validate the retained patch, its postimage,
// and its attempt-bound verification while holding the integration lock.
func (s *SQLStore) ReconcileAppliedReceipt(ctx context.Context, p Plan, nodeID NodeID, attemptID AttemptID, artifactID, receiptID string, expectedVersion int64) error {
	if nodeID == "" || attemptID == "" || artifactID == "" || receiptID == "" {
		return ErrPlanReceiptNotEligible
	}
	if err := validateArtifactID(artifactID); err != nil {
		return err
	}
	if err := validateReceiptID(receiptID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin plan receipt reconciliation: %w", err)
	}
	defer tx.Rollback()
	var state PlanState
	var reason StopReason
	var version int64
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT state, stop_reason, version FROM plans`), planArgs(p)...).Scan(&state, &reason, &version); err != nil {
		return fmt.Errorf("load plan for receipt reconciliation: %w", err)
	}
	if version != expectedVersion {
		return ErrStaleVersion
	}
	var nodeState NodeState
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT state FROM plan_nodes`)+` AND node_id = ?`, append(planArgs(p), nodeID)...).Scan(&nodeState); err != nil {
		return fmt.Errorf("load reconciled node: %w", err)
	}
	if nodeState == NodeCompleted {
		var storedArtifact, storedReceipt string
		if err := tx.QueryRowContext(ctx, planWhere(`SELECT artifact_id, receipt_id FROM plan_artifacts`)+` AND node_id = ?`, append(planArgs(p), nodeID)...).Scan(&storedArtifact, &storedReceipt); err == nil && storedArtifact == artifactID && storedReceipt == receiptID {
			return nil
		}
		return ErrPlanReceiptNotEligible
	}
	if state != PlanPaused || reason != StopAmbiguity || nodeState != NodeBlocked {
		return ErrPlanReceiptNotEligible
	}
	var attempts, totalAttempts, leases, artifacts int
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT COUNT(*) FROM plan_attempts`)+` AND node_id = ? AND attempt_id = ?`, append(planArgs(p), nodeID, attemptID)...).Scan(&attempts); err != nil {
		return fmt.Errorf("check receipt attempt: %w", err)
	}
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT COUNT(*) FROM plan_attempts`)+` AND node_id = ?`, append(planArgs(p), nodeID)...).Scan(&totalAttempts); err != nil {
		return fmt.Errorf("check other node attempts: %w", err)
	}
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT COUNT(*) FROM plan_leases`), planArgs(p)...).Scan(&leases); err != nil {
		return fmt.Errorf("check plan owners: %w", err)
	}
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT COUNT(*) FROM plan_artifacts`)+` AND node_id = ?`, append(planArgs(p), nodeID)...).Scan(&artifacts); err != nil {
		return fmt.Errorf("check existing artifact: %w", err)
	}
	if attempts != 1 || totalAttempts != 1 || leases != 0 || artifacts != 0 {
		return ErrPlanReceiptNotEligible
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_artifacts (tenant_id, repository_id, task_id, plan_id, generation_id, node_id, artifact_id, receipt_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, append(planArgs(p), nodeID, artifactID, receiptID)...); err != nil {
		return fmt.Errorf("persist reconciled artifact: %w", err)
	}
	if err := updateNode(ctx, tx, p, nodeID, NodeCompleted); err != nil {
		return err
	}
	key := "reconcile-receipt:" + receiptID
	if err := appendEvent(ctx, tx, p, EventID(key), nodeID, IdempotencyKey(key)); err != nil {
		return err
	}
	var blocked int
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT COUNT(*) FROM plan_nodes`)+` AND state = 'blocked'`, planArgs(p)...).Scan(&blocked); err != nil {
		return fmt.Errorf("check other blocked nodes: %w", err)
	}
	if blocked == 0 {
		if _, err := tx.ExecContext(ctx, planWhere(`UPDATE plans SET state = 'active', stop_reason = ''`), planArgs(p)...); err != nil {
			return fmt.Errorf("resume reconciled plan: %w", err)
		}
		if err := promoteReady(ctx, tx, p); err != nil {
			return err
		}
		if err := derivePlanState(ctx, tx, p); err != nil {
			return err
		}
	}
	changed, err := tx.ExecContext(ctx, planWhere(`UPDATE plans SET version = version + 1`)+` AND version = ?`, append(planArgs(p), expectedVersion)...)
	if err != nil {
		return fmt.Errorf("advance reconciled plan version: %w", err)
	}
	if rows, err := changed.RowsAffected(); err != nil || rows != 1 {
		return ErrStaleVersion
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit plan receipt reconciliation: %w", err)
	}
	return nil
}
