package plan

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrNoReadyNode = errors.New("no ready plan node")
	ErrLeaseLost   = errors.New("plan node lease lost")
)

// SchedulerConfig bounds concurrent work and total attempts for a node.
type SchedulerConfig struct {
	MaxConcurrent int
	MaxAttempts   int
	LeaseDuration time.Duration
	Now           func() time.Time
}

const defaultPlanLeaseDuration = 15 * time.Minute

const (
	defaultPlanMaxConcurrent = 3
	defaultPlanMaxAttempts   = 3
)

func (s *Scheduler) now() time.Time {
	if s.config.Now != nil {
		return s.config.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Scheduler) leaseDuration() time.Duration {
	if s.config.LeaseDuration > 0 {
		return s.config.LeaseDuration
	}
	return defaultPlanLeaseDuration
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
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = defaultPlanMaxConcurrent
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = defaultPlanMaxAttempts
	}
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
	expired, err := reapExpiredPlanLeases(ctx, tx, p, s.now())
	if err != nil {
		return nil, err
	}
	if expired {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit expired plan leases: %w", err)
		}
		return nil, nil
	}
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
	expired, err := reapExpiredPlanLeases(ctx, tx, p, s.now())
	if err != nil {
		return Node{}, err
	}
	if expired {
		if err := tx.Commit(); err != nil {
			return Node{}, fmt.Errorf("commit expired plan leases: %w", err)
		}
		return Node{}, ErrNoReadyNode
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
	var risks string
	err = tx.QueryRowContext(ctx, planWhere(`SELECT node_id, state, scope, risks, verification FROM plan_nodes`)+` AND state = 'ready' ORDER BY node_id LIMIT 1`, planArgs(p)...).Scan(&node.ID, &node.State, &node.Scope, &risks, &node.Verification)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNoReadyNode
	}
	if err != nil {
		return Node{}, fmt.Errorf("select ready plan node: %w", err)
	}
	if err := json.Unmarshal([]byte(risks), &node.Risks); err != nil {
		return Node{}, fmt.Errorf("decode ready plan node risks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, planWhere(`UPDATE plan_nodes SET state = 'leased'`)+` AND node_id = ? AND state = 'ready'`, append(planArgs(p), node.ID)...); err != nil {
		return Node{}, fmt.Errorf("lease plan node: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_attempts (tenant_id, repository_id, task_id, plan_id, generation_id, attempt_id, node_id, idempotency_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, append(planArgs(p), request.AttemptID, node.ID, request.IdempotencyKey)...); err != nil {
		return Node{}, fmt.Errorf("insert plan attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_leases (tenant_id, repository_id, task_id, plan_id, generation_id, lease_id, attempt_id, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, append(planArgs(p), request.LeaseID, request.AttemptID, s.now().Add(s.leaseDuration()).Format(time.RFC3339Nano))...); err != nil {
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
	now := s.now()
	if err := leasedNode(ctx, tx, p, nodeID, leaseID, now, NodeLeased, NodeRunning); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, planWhere(`UPDATE plan_leases SET expires_at = ?`)+` AND lease_id = ? AND expires_at > ?`, append([]any{now.Add(s.leaseDuration()).Format(time.RFC3339Nano)}, append(planArgs(p), leaseID, now.Format(time.RFC3339Nano))...)...)
	if err != nil {
		return fmt.Errorf("renew plan lease: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return ErrLeaseLost
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
	return s.CompleteWithEvidence(ctx, p, nodeID, leaseID, eventID, key, nil)
}

// CompleteWithEvidence commits evidence references and node completion in one transaction.
func (s *Scheduler) CompleteWithEvidence(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey, evidenceIDs []EvidenceID) error {
	return s.CompleteWithArtifact(ctx, p, nodeID, leaseID, eventID, key, evidenceIDs, "")
}

// CompleteWithArtifact binds the accepted patch to its node in the same fenced
// transaction that commits node completion and releases the owner lease.
func (s *Scheduler) CompleteWithArtifact(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey, evidenceIDs []EvidenceID, artifactID string) error {
	if artifactID != "" {
		if err := validateArtifactID(artifactID); err != nil {
			return err
		}
	}
	return s.finish(ctx, p, nodeID, leaseID, eventID, key, NodeCompleted, "complete", evidenceIDs, artifactID)
}

func validateArtifactID(artifactID string) error {
	if !strings.HasPrefix(artifactID, "sha256:") || len(artifactID) != 71 {
		return fmt.Errorf("invalid plan artifact identity")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(artifactID, "sha256:")); err != nil {
		return fmt.Errorf("invalid plan artifact identity: %w", err)
	}
	return nil
}

func (s *Scheduler) Block(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey) error {
	return s.finish(ctx, p, nodeID, leaseID, eventID, key, NodeBlocked, "block", nil, "")
}

func (s *Scheduler) Fail(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey) error {
	return s.finish(ctx, p, nodeID, leaseID, eventID, key, NodeFailed, "fail", nil, "")
}

func (s *Scheduler) Cancel(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey) error {
	return s.finish(ctx, p, nodeID, leaseID, eventID, key, NodeCanceled, "cancel", nil, "")
}

// Stop atomically persists a typed terminal result and its plan stop state.
func (s *Scheduler) Stop(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey, status NodeState, reason StopReason) error {
	if reason == "" || (status != NodeFailed && status != NodeBlocked && status != NodeCanceled) {
		return fmt.Errorf("stop node: invalid terminal result")
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin stop node: %w", err)
	}
	defer tx.Rollback()
	if err := leasedNode(ctx, tx, p, nodeID, leaseID, s.now(), NodeLeased, NodeRunning); err != nil {
		return err
	}
	if err := releaseLease(ctx, tx, p, leaseID); err != nil {
		return err
	}
	if err := updateNode(ctx, tx, p, nodeID, status); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, p, eventID, nodeID, key); err != nil {
		return err
	}
	state := PlanPaused
	if status == NodeFailed {
		state = PlanFailed
		if err := blockDependents(ctx, tx, p, nodeID); err != nil {
			return err
		}
	} else if status == NodeCanceled {
		state = PlanCanceled
		if err := cancelRemaining(ctx, tx, p); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, planWhere(`UPDATE plans SET state = ?, stop_reason = ?`), append([]any{state, reason}, planArgs(p)...)...); err != nil {
		return fmt.Errorf("persist plan stop: %w", err)
	}
	if err := bumpVersion(ctx, tx, p); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit stop node: %w", err)
	}
	return nil
}

// Retry records a failed attempt. Exhaustion fails the node and plan instead.
func (s *Scheduler) Retry(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin retry node: %w", err)
	}
	defer tx.Rollback()
	if err := leasedNode(ctx, tx, p, nodeID, leaseID, s.now(), NodeLeased, NodeRunning); err != nil {
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
	if err := leasedNode(ctx, tx, p, nodeID, leaseID, s.now(), NodeLeased); err != nil {
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

func (s *Scheduler) finish(ctx context.Context, p Plan, nodeID NodeID, leaseID LeaseID, eventID EventID, key IdempotencyKey, next NodeState, operation string, evidenceIDs []EvidenceID, artifactID string) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s node: %w", operation, err)
	}
	defer tx.Rollback()
	if err := leasedNode(ctx, tx, p, nodeID, leaseID, s.now(), NodeLeased, NodeRunning); err != nil {
		return err
	}
	if err := releaseLease(ctx, tx, p, leaseID); err != nil {
		return err
	}
	for _, evidenceID := range evidenceIDs {
		if evidenceID == "" {
			return fmt.Errorf("insert plan evidence: missing evidence identity")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_evidence (tenant_id, repository_id, task_id, plan_id, generation_id, evidence_id, node_id) VALUES (?, ?, ?, ?, ?, ?, ?)`, append(planArgs(p), evidenceID, nodeID)...); err != nil {
			return fmt.Errorf("insert plan evidence: %w", err)
		}
	}
	if artifactID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_artifacts (tenant_id, repository_id, task_id, plan_id, generation_id, node_id, artifact_id) VALUES (?, ?, ?, ?, ?, ?, ?)`, append(planArgs(p), nodeID, artifactID)...); err != nil {
			return fmt.Errorf("persist accepted plan artifact: %w", err)
		}
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
	rows, err := tx.QueryContext(ctx, planWhere(`SELECT node_id, state, scope, risks, verification FROM plan_nodes`)+` AND state = 'ready' ORDER BY node_id`, planArgs(p)...)
	if err != nil {
		return nil, fmt.Errorf("query ready plan nodes: %w", err)
	}
	defer rows.Close()
	var nodes []Node
	for rows.Next() {
		var node Node
		var risks string
		if err := rows.Scan(&node.ID, &node.State, &node.Scope, &risks, &node.Verification); err != nil {
			return nil, fmt.Errorf("scan ready plan node: %w", err)
		}
		if err := json.Unmarshal([]byte(risks), &node.Risks); err != nil {
			return nil, fmt.Errorf("decode ready plan node risks: %w", err)
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func leasedNode(ctx context.Context, tx *sql.Tx, p Plan, nodeID NodeID, leaseID LeaseID, now time.Time, states ...NodeState) error {
	query := `SELECT n.state FROM plan_nodes n JOIN plan_attempts a ON a.tenant_id = n.tenant_id AND a.repository_id = n.repository_id AND a.task_id = n.task_id AND a.plan_id = n.plan_id AND a.generation_id = n.generation_id AND a.node_id = n.node_id JOIN plan_leases l ON l.tenant_id = a.tenant_id AND l.repository_id = a.repository_id AND l.task_id = a.task_id AND l.plan_id = a.plan_id AND l.generation_id = a.generation_id AND l.attempt_id = a.attempt_id WHERE n.tenant_id = ? AND n.repository_id = ? AND n.task_id = ? AND n.plan_id = ? AND n.generation_id = ? AND n.node_id = ? AND l.lease_id = ? AND l.expires_at > ?`
	var state NodeState
	err := tx.QueryRowContext(ctx, query, append(append(planArgs(p), nodeID), leaseID, now.Format(time.RFC3339Nano))...).Scan(&state)
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

// Expired plan leases have unknown effect outcomes. Park their nodes for
// reconciliation instead of re-executing an arbitrary shell or API mutation.
func reapExpiredPlanLeases(ctx context.Context, tx *sql.Tx, p Plan, now time.Time) (bool, error) {
	query := `SELECT l.lease_id, a.node_id FROM plan_leases l JOIN plan_attempts a ON a.tenant_id = l.tenant_id AND a.repository_id = l.repository_id AND a.task_id = l.task_id AND a.plan_id = l.plan_id AND a.generation_id = l.generation_id AND a.attempt_id = l.attempt_id WHERE l.tenant_id = ? AND l.repository_id = ? AND l.task_id = ? AND l.plan_id = ? AND l.generation_id = ? AND l.expires_at <= ? ORDER BY l.lease_id`
	rows, err := tx.QueryContext(ctx, query, append(planArgs(p), now.Format(time.RFC3339Nano))...)
	if err != nil {
		return false, fmt.Errorf("find expired plan leases: %w", err)
	}
	type expiredLease struct {
		id   LeaseID
		node NodeID
	}
	var expired []expiredLease
	for rows.Next() {
		var lease expiredLease
		if err := rows.Scan(&lease.id, &lease.node); err != nil {
			rows.Close()
			return false, fmt.Errorf("scan expired plan lease: %w", err)
		}
		expired = append(expired, lease)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("read expired plan leases: %w", err)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("close expired plan leases: %w", err)
	}
	for _, lease := range expired {
		if err := updateNode(ctx, tx, p, lease.node, NodeBlocked); err != nil {
			return false, err
		}
		if err := releaseLease(ctx, tx, p, lease.id); err != nil {
			return false, err
		}
		key := "lease-expired:" + string(lease.id)
		if err := appendEvent(ctx, tx, p, EventID(key), lease.node, IdempotencyKey(key)); err != nil {
			return false, err
		}
	}
	if len(expired) == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, planWhere(`UPDATE plans SET state = ?, stop_reason = ?`), append([]any{PlanPaused, StopAmbiguity}, planArgs(p)...)...); err != nil {
		return false, fmt.Errorf("pause plan after expired lease: %w", err)
	}
	if err := bumpVersion(ctx, tx, p); err != nil {
		return false, err
	}
	return true, nil
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
