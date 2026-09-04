package plan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	ErrNoReadyNode = errors.New("no ready plan node")
	ErrLeaseLost   = errors.New("plan node lease lost")
)

// SchedulerConfig bounds concurrent work and total attempts for a node.
type SchedulerConfig struct {
	MaxConcurrent int
	MaxAttempts   int
}

// ClaimRequest supplies the durable identities created by a successful claim.
type ClaimRequest struct {
	AttemptID      AttemptID
	LeaseID        LeaseID
	EventID        EventID
	IdempotencyKey IdempotencyKey
}

// Scheduler advances one plan generation at a time.
type Scheduler struct {
	store  *SQLStore
	config SchedulerConfig
}

func NewScheduler(store *SQLStore, config SchedulerConfig) *Scheduler {
	return &Scheduler{store: store, config: config}
}

// Ready promotes dependency-satisfied pending and retrying nodes, then returns
// the currently claimable nodes.
func (s *Scheduler) Ready(ctx context.Context, p Plan) ([]Node, error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin ready nodes: %w", err)
	}
	defer tx.Rollback()
	if err := promoteReady(ctx, tx, p); err != nil {
		return nil, err
	}
	nodes, err := readyNodes(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit ready nodes: %w", err)
	}
	return nodes, nil
}

// Claim atomically promotes dependency-ready work and leases one node.
func (s *Scheduler) Claim(ctx context.Context, p Plan, request ClaimRequest) (Node, error) {
	if request.AttemptID == "" || request.LeaseID == "" || request.EventID == "" || request.IdempotencyKey == "" {
		return Node{}, fmt.Errorf("claim node: missing durable identity")
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return Node{}, fmt.Errorf("begin claim node: %w", err)
	}
	defer tx.Rollback()
	if err := activePlan(ctx, tx, p); err != nil {
		return Node{}, err
	}
	if err := promoteReady(ctx, tx, p); err != nil {
		return Node{}, err
	}
	if s.config.MaxConcurrent > 0 {
		var active int
		if err := tx.QueryRowContext(ctx, planWhere(`SELECT COUNT(*) FROM plan_nodes`)+` AND state IN ('leased', 'running')`, planArgs(p)...).Scan(&active); err != nil {
			return Node{}, fmt.Errorf("count active plan nodes: %w", err)
		}
		if active >= s.config.MaxConcurrent {
			return Node{}, ErrNoReadyNode
		}
	}
	var node Node
	err = tx.QueryRowContext(ctx, planWhere(`SELECT node_id, state FROM plan_nodes`)+` AND state = 'ready' ORDER BY node_id LIMIT 1`, planArgs(p)...).Scan(&node.ID, &node.State)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNoReadyNode
	}
	if err != nil {
		return Node{}, fmt.Errorf("select ready plan node: %w", err)
	}
	if _, err := tx.ExecContext(ctx, planWhere(`UPDATE plan_nodes SET state = 'leased'`)+` AND node_id = ? AND state = 'ready'`, append(planArgs(p), node.ID)...); err != nil {
		return Node{}, fmt.Errorf("lease plan node: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_attempts (tenant_id, repository_id, task_id, plan_id, generation_id, attempt_id, node_id, idempotency_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, append(planArgs(p), request.AttemptID, node.ID, request.IdempotencyKey)...); err != nil {
		return Node{}, fmt.Errorf("insert plan attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_leases (tenant_id, repository_id, task_id, plan_id, generation_id, lease_id, attempt_id) VALUES (?, ?, ?, ?, ?, ?, ?)`, append(planArgs(p), request.LeaseID, request.AttemptID)...); err != nil {
		return Node{}, fmt.Errorf("insert plan lease: %w", err)
	}
	if err := appendEvent(ctx, tx, p, request.EventID, node.ID, request.IdempotencyKey); err != nil {
		return Node{}, err
	}
	if err := bumpVersion(ctx, tx, p); err != nil {
		return Node{}, err
	}
	if err := tx.Commit(); err != nil {
		return Node{}, fmt.Errorf("commit claim node: %w", err)
	}
	node.State = NodeLeased
	return node, nil
}

func (s *Scheduler) Heartbeat(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin heartbeat: %w", err)
	}
	defer tx.Rollback()
	if err := leasedNode(ctx, tx, p, nodeID, leaseID, NodeLeased, NodeRunning); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit heartbeat: %w", err)
	}
	return nil
}

func (s *Scheduler) Start(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID) error {
	return s.transitionLeased(ctx, p, nodeID, leaseID, NodeRunning, "start")
}

func (s *Scheduler) Complete(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey) error {
	return s.finish(ctx, p, nodeID, leaseID, eventID, key, NodeCompleted, "complete")
}

func (s *Scheduler) Block(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey) error {
	return s.finish(ctx, p, nodeID, leaseID, eventID, key, NodeBlocked, "block")
}

func (s *Scheduler) Fail(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey) error {
	return s.finish(ctx, p, nodeID, leaseID, eventID, key, NodeFailed, "fail")
}

func (s *Scheduler) Cancel(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey) error {
	return s.finish(ctx, p, nodeID, leaseID, eventID, key, NodeCanceled, "cancel")
}

// Retry records a failed attempt. Exhaustion fails the node and plan instead.
func (s *Scheduler) Retry(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin retry node: %w", err)
	}
	defer tx.Rollback()
	if err := leasedNode(ctx, tx, p, nodeID, leaseID, NodeLeased, NodeRunning); err != nil {
		return err
	}
	if err := releaseLease(ctx, tx, p, leaseID); err != nil {
		return err
	}
	var attempts int
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT COUNT(*) FROM plan_attempts`)+` AND node_id = ?`, append(planArgs(p), nodeID)...).Scan(&attempts); err != nil {
		return fmt.Errorf("count plan attempts: %w", err)
	}
	next := NodeRetryWait
	if s.config.MaxAttempts > 0 && attempts >= s.config.MaxAttempts {
		next = NodeFailed
	}
	if err := updateNode(ctx, tx, p, nodeID, next); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, p, eventID, nodeID, key); err != nil {
		return err
	}
	if next == NodeFailed {
		if err := blockDependents(ctx, tx, p, nodeID); err != nil {
			return err
		}
	}
	if err := derivePlanState(ctx, tx, p); err != nil {
		return err
	}
	if err := bumpVersion(ctx, tx, p); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit retry node: %w", err)
	}
	return nil
}

func (s *Scheduler) transitionLeased(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, next NodeState, operation string) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s node: %w", operation, err)
	}
	defer tx.Rollback()
	if err := leasedNode(ctx, tx, p, nodeID, leaseID, NodeLeased); err != nil {
		return err
	}
	if err := updateNode(ctx, tx, p, nodeID, next); err != nil {
		return err
	}
	if err := bumpVersion(ctx, tx, p); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s node: %w", operation, err)
	}
	return nil
}

func (s *Scheduler) finish(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey, next NodeState, operation string) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s node: %w", operation, err)
	}
	defer tx.Rollback()
	if err := leasedNode(ctx, tx, p, nodeID, leaseID, NodeLeased, NodeRunning); err != nil {
		return err
	}
	if err := releaseLease(ctx, tx, p, leaseID); err != nil {
		return err
	}
	if err := updateNode(ctx, tx, p, nodeID, next); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, p, eventID, nodeID, key); err != nil {
		return err
	}
	if next == NodeCompleted {
		if err := promoteReady(ctx, tx, p); err != nil {
			return err
		}
	}
	if next == NodeFailed {
		if err := blockDependents(ctx, tx, p, nodeID); err != nil {
			return err
		}
	}
	if next == NodeCanceled {
		if err := cancelRemaining(ctx, tx, p); err != nil {
			return err
		}
	}
	if err := derivePlanState(ctx, tx, p); err != nil {
		return err
	}
	if err := bumpVersion(ctx, tx, p); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s node: %w", operation, err)
	}
	return nil
}

func activePlan(ctx context.Context, tx *sql.Tx, p Plan) error {
	var state PlanState
	if err := tx.QueryRowContext(ctx, planWhere(`SELECT state FROM plans`), planArgs(p)...).Scan(&state); err != nil {
		return fmt.Errorf("read plan state: %w", err)
	}
	if state != PlanActive {
		return ErrNoReadyNode
	}
	return nil
}

func promoteReady(ctx context.Context, tx *sql.Tx, p Plan) error {
	_, err := tx.ExecContext(ctx, planWhere(`UPDATE plan_nodes AS n SET state = 'ready'`)+` AND n.state IN ('pending', 'retry_wait') AND NOT EXISTS (SELECT 1 FROM plan_edges e JOIN plan_nodes predecessor ON predecessor.tenant_id = e.tenant_id AND predecessor.repository_id = e.repository_id AND predecessor.task_id = e.task_id AND predecessor.plan_id = e.plan_id AND predecessor.generation_id = e.generation_id AND predecessor.node_id = e.depends_on WHERE e.tenant_id = n.tenant_id AND e.repository_id = n.repository_id AND e.task_id = n.task_id AND e.plan_id = n.plan_id AND e.generation_id = n.generation_id AND e.node_id = n.node_id AND predecessor.state != 'completed')`, planArgs(p)...)
	if err != nil {
		return fmt.Errorf("promote ready plan nodes: %w", err)
	}
	return nil
}

func readyNodes(ctx context.Context, tx *sql.Tx, p Plan) ([]Node, error) {
	rows, err := tx.QueryContext(ctx, planWhere(`SELECT node_id, state FROM plan_nodes`)+` AND state = 'ready' ORDER BY node_id`, planArgs(p)...)
	if err != nil {
		return nil, fmt.Errorf("query ready plan nodes: %w", err)
	}
	defer rows.Close()
	var nodes []Node
	for rows.Next() {
		var node Node
		if err := rows.Scan(&node.ID, &node.State); err != nil {
			return nil, fmt.Errorf("scan ready plan node: %w", err)
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func leasedNode(ctx context.Context, tx *sql.Tx, p Plan, nodeID NodeID, leaseID LeaseID, states ...NodeState) error {
	query := `SELECT n.state FROM plan_nodes n JOIN plan_attempts a ON a.tenant_id = n.tenant_id AND a.repository_id = n.repository_id AND a.task_id = n.task_id AND a.plan_id = n.plan_id AND a.generation_id = n.generation_id AND a.node_id = n.node_id JOIN plan_leases l ON l.tenant_id = a.tenant_id AND l.repository_id = a.repository_id AND l.task_id = a.task_id AND l.plan_id = a.plan_id AND l.generation_id = a.generation_id AND l.attempt_id = a.attempt_id WHERE n.tenant_id = ? AND n.repository_id = ? AND n.task_id = ? AND n.plan_id = ? AND n.generation_id = ? AND n.node_id = ? AND l.lease_id = ?`
	var state NodeState
	err := tx.QueryRowContext(ctx, query, append(append(planArgs(p), nodeID), leaseID)...).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("read leased plan node: %w", err)
	}
	for _, allowed := range states {
		if state == allowed {
			return nil
		}
	}
	return ErrLeaseLost
}

func updateNode(ctx context.Context, tx *sql.Tx, p Plan, nodeID NodeID, state NodeState) error {
	if _, err := tx.ExecContext(ctx, planWhere(`UPDATE plan_nodes SET state = ?`)+` AND node_id = ?`, append([]any{state}, append(planArgs(p), nodeID)...)...); err != nil {
		return fmt.Errorf("update plan node: %w", err)
	}
	return nil
}

func releaseLease(ctx context.Context, tx *sql.Tx, p Plan, leaseID LeaseID) error {
	if _, err := tx.ExecContext(ctx, planWhere(`DELETE FROM plan_leases`)+` AND lease_id = ?`, append(planArgs(p), leaseID)...); err != nil {
		return fmt.Errorf("release plan lease: %w", err)
	}
	return nil
}

func appendEvent(ctx context.Context, tx *sql.Tx, p Plan, eventID EventID, nodeID NodeID, key IdempotencyKey) error {
	if eventID == "" || key == "" {
		return fmt.Errorf("append plan event: missing durable identity")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_events (tenant_id, repository_id, task_id, plan_id, generation_id, event_id, node_id, idempotency_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, append(planArgs(p), eventID, nodeID, key)...); err != nil {
		return fmt.Errorf("append plan event: %w", err)
	}
	return nil
}

func blockDependents(ctx context.Context, tx *sql.Tx, p Plan, nodeID NodeID) error {
	_, err := tx.ExecContext(ctx, planWhere(`UPDATE plan_nodes SET state = 'blocked'`)+` AND state IN ('pending', 'ready', 'retry_wait') AND node_id IN (SELECT node_id FROM plan_edges WHERE tenant_id = ? AND repository_id = ? AND task_id = ? AND plan_id = ? AND generation_id = ? AND depends_on = ?)`, append(planArgs(p), append(planArgs(p), nodeID)...)...)
	if err != nil {
		return fmt.Errorf("block dependent plan nodes: %w", err)
	}
	return nil
}

func cancelRemaining(ctx context.Context, tx *sql.Tx, p Plan) error {
	if _, err := tx.ExecContext(ctx, planWhere(`UPDATE plan_nodes SET state = 'canceled'`)+` AND state != 'completed'`, planArgs(p)...); err != nil {
		return fmt.Errorf("cancel remaining plan nodes: %w", err)
	}
	return nil
}

func derivePlanState(ctx context.Context, tx *sql.Tx, p Plan) error {
	rows, err := tx.QueryContext(ctx, planWhere(`SELECT node_id, state FROM plan_nodes`), planArgs(p)...)
	if err != nil {
		return fmt.Errorf("read plan nodes for terminal state: %w", err)
	}
	defer rows.Close()
	var nodes []Node
	for rows.Next() {
		var node Node
		if err := rows.Scan(&node.ID, &node.State); err != nil {
			return fmt.Errorf("scan plan node for terminal state: %w", err)
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read plan nodes for terminal state: %w", err)
	}
	state, terminal := DeriveTerminalState(nodes)
	if !terminal {
		return nil
	}
	if _, err := tx.ExecContext(ctx, planWhere(`UPDATE plans SET state = ?`), append([]any{state}, planArgs(p)...)...); err != nil {
		return fmt.Errorf("derive terminal plan state: %w", err)
	}
	return nil
}

func bumpVersion(ctx context.Context, tx *sql.Tx, p Plan) error {
	if _, err := tx.ExecContext(ctx, planWhere(`UPDATE plans SET version = version + 1`), planArgs(p)...); err != nil {
		return fmt.Errorf("advance plan version: %w", err)
	}
	return nil
}
