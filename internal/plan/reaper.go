package plan

import (
	"context"
	"fmt"
	"time"
)

// ReapExpiredLeases scans a bounded prefix of active/waiting plan generations.
// This is an internal supervisor operation; it never returns another tenant's
// plan content to an HTTP caller. Unknown effects are parked, not replayed.
func (s *SQLStore) ReapExpiredLeases(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 || limit > 1000 {
		return 0, fmt.Errorf("plan reaper limit must be between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT l.tenant_id, l.repository_id, l.task_id, l.plan_id, l.generation_id
		FROM plan_leases l JOIN plans p ON p.tenant_id = l.tenant_id AND p.repository_id = l.repository_id
		AND p.task_id = l.task_id AND p.plan_id = l.plan_id AND p.generation_id = l.generation_id
		WHERE p.state IN ('active', 'paused') AND l.expires_at <= ?
		ORDER BY l.tenant_id, l.repository_id, l.task_id, l.plan_id, l.generation_id LIMIT ?`, now.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return 0, fmt.Errorf("list expired plan generations: %w", err)
	}
	var plans []Plan
	for rows.Next() {
		var p Plan
		if err := rows.Scan(&p.TenantID, &p.RepositoryID, &p.TaskID, &p.ID, &p.Generation); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired plan generation: %w", err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("list expired plan generations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close expired plan generation list: %w", err)
	}
	reaped := 0
	for _, p := range plans {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return reaped, fmt.Errorf("begin plan lease recovery: %w", err)
		}
		changed, err := reapExpiredPlanLeases(ctx, tx, p, now.UTC())
		if err != nil {
			_ = tx.Rollback()
			return reaped, err
		}
		if err := tx.Commit(); err != nil {
			return reaped, fmt.Errorf("commit plan lease recovery: %w", err)
		}
		if changed {
			reaped++
		}
	}
	return reaped, nil
}
