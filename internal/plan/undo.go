package plan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrArtifactUndoBlocked = errors.New("plan artifact cannot be undone while dependent work is accepted")

// ArtifactUndoEligible validates the scoped leaf and returns the plan version
// before the filesystem effect. Only an operator can request this transition;
// the worktree manager independently verifies the retained patch/postimage.
func (s *SQLStore) ArtifactUndoEligible(ctx context.Context, p Plan, nodeID NodeID, receiptID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin plan artifact undo validation: %w", err)
	}
	defer tx.Rollback()
	return undoEligible(ctx, tx, p, nodeID, receiptID)
}

func undoEligible(ctx context.Context, tx *sql.Tx, p Plan, nodeID NodeID, receiptID string) (int64, error) {
	var state PlanState
	var version int64
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT state, version FROM plans`), planArgs(p)...).Scan(&state, &version); err != nil {
		return 0, fmt.Errorf("load plan for undo: %w", err)
	}
	var artifactID, storedReceipt string
	var undone bool
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT artifact_id, receipt_id, undone FROM plan_artifacts`)+` AND node_id = ?`, append(planArgs(p), nodeID)...).Scan(&artifactID, &storedReceipt, &undone); err != nil {
		return 0, fmt.Errorf("load plan artifact for undo: %w", err)
	}
	if artifactID == "" || storedReceipt == "" || storedReceipt != receiptID {
		return 0, ErrArtifactUndoBlocked
	}
	if undone {
		return version, nil
	}
	if state != PlanCompleted && state != PlanPaused {
		return 0, ErrArtifactUndoBlocked
	}
	var nodeState NodeState
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT state FROM plan_nodes`)+` AND node_id = ?`, append(planArgs(p), nodeID)...).Scan(&nodeState); err != nil || nodeState != NodeCompleted {
		return 0, ErrArtifactUndoBlocked
	}
	var leases int
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT COUNT(*) FROM plan_leases`), planArgs(p)...).Scan(&leases); err != nil {
		return 0, fmt.Errorf("check active plan owners before undo: %w", err)
	}
	if leases != 0 {
		return 0, ErrArtifactUndoBlocked
	}
	query := planWhere(`SELECT COUNT(*) FROM plan_nodes`) + ` AND state = 'completed' AND node_id IN (SELECT node_id FROM plan_edges WHERE tenant_id = ? AND repository_id = ? AND task_id = ? AND plan_id = ? AND generation_id = ? AND depends_on = ?)`
	var dependents int
	if err := tx.QueryRowContext(ctx, query, append(planArgs(p), append(planArgs(p), nodeID)...)...).Scan(&dependents); err != nil {
		return 0, fmt.Errorf("check accepted plan dependents before undo: %w", err)
	}
	if dependents != 0 {
		return 0, ErrArtifactUndoBlocked
	}
	return version, nil
}

// RecordArtifactUndo is the compensating plan transaction after the worktree
// undo receipt proves the bytes were restored. Retain the original artifact
// lineage, invalidate completion and require an explicit successor generation.
func (s *SQLStore) RecordArtifactUndo(ctx context.Context, p Plan, nodeID NodeID, receiptID string, expectedVersion int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin plan artifact undo: %w", err)
	}
	defer tx.Rollback()
	version, err := undoEligible(ctx, tx, p, nodeID, receiptID)
	if err != nil {
		return err
	}
	if version != expectedVersion {
		return ErrStaleVersion
	}
	var undone bool
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT undone FROM plan_artifacts`)+` AND node_id = ?`, append(planArgs(p), nodeID)...).Scan(&undone); err != nil {
		return fmt.Errorf("read artifact undo state: %w", err)
	}
	if undone {
		return nil
	}
	if _, err := tx.ExecContext(ctx, planWhere(`UPDATE plan_artifacts SET undone = 1`)+` AND node_id = ? AND undone = 0`, append(planArgs(p), nodeID)...); err != nil {
		return fmt.Errorf("mark accepted artifact undone: %w", err)
	}
	if err := updateNode(ctx, tx, p, nodeID, NodeBlocked); err != nil {
		return err
	}
	key := "artifact-undone:" + string(nodeID) + ":" + receiptID
	if err := appendEvent(ctx, tx, p, EventID(key), nodeID, IdempotencyKey(key)); err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, planWhere(`UPDATE plans SET state = ?, stop_reason = ?, version = version + 1`)+` AND version = ?`, append([]any{PlanReplanning, StopUserDecisionRequired}, append(planArgs(p), expectedVersion)...)...)
	if err != nil {
		return fmt.Errorf("invalidate plan after artifact undo: %w", err)
	}
	if changed, err := updated.RowsAffected(); err != nil || changed != 1 {
		return ErrStaleVersion
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit plan artifact undo: %w", err)
	}
	return nil
}
