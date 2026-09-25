package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNoRunnableDelivery = errors.New("no runnable delivery")
	ErrStaleLease         = errors.New("stale delivery lease")
	ErrDeliveryCancelled  = errors.New("delivery cancelled")
)

type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type Lease struct {
	Delivery  Delivery
	OwnerID   string
	Epoch     int64
	Attempt   int64
	Acquired  time.Time
	Expires   time.Time
	Heartbeat time.Time
}

type AttemptRecord struct {
	Attempt    int64
	OwnerID    string
	LeaseEpoch int64
	StartedAt  time.Time
	FinishedAt *time.Time
	Outcome    OutcomeKind
	Checkpoint json.RawMessage
	Result     json.RawMessage
	Error      string
}

type OutcomeKind string

const (
	OutcomeSucceeded OutcomeKind = "succeeded"
	OutcomeFailed    OutcomeKind = "failed"
	OutcomeRetry     OutcomeKind = "retry"
	OutcomeWaiting   OutcomeKind = "waiting"
	OutcomeCancelled OutcomeKind = "cancelled"
	OutcomeLeaseLost OutcomeKind = "lease_expired"
)

type Outcome struct {
	Kind              OutcomeKind
	Result            json.RawMessage
	Err               error
	RetryAt           time.Time
	WaitState         DeliveryState
	Signal            string
	ActiveNanoseconds int64
}

// QueueAdmitted promotes an already persisted admission only after a worker
// service has been installed. State, queue record, and ordered event commit in
// the same SQLite transaction; a crash cannot leave a falsely queued record.
func (s *DeliveryStore) QueueAdmitted(ctx context.Context, scope DeliveryScope, id DeliveryID, expectedVersion int64) (Delivery, error) {
	if err := validateDeliveryRef(scope, id); err != nil || expectedVersion < 1 {
		return Delivery{}, ErrInvalidDelivery
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, fmt.Errorf("begin delivery queue promotion: %w", err)
	}
	defer tx.Rollback()
	delivery, err := loadDelivery(ctx, tx, scope, id)
	if err != nil {
		return Delivery{}, err
	}
	if delivery.Version != expectedVersion {
		return Delivery{}, ErrStaleDeliveryVersion
	}
	if delivery.State != DeliveryAdmitted {
		return Delivery{}, ErrInvalidDeliveryTransition
	}
	now := s.clock.Now().UTC()
	if err := transitionInTx(ctx, tx, &delivery, DeliveryQueued, queueEventIdentity(id, "promote", expectedVersion, now)); err != nil {
		return Delivery{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_queue (tenant_id, repository_id, delivery_id, status, available_at, created_at, updated_at) VALUES (?, ?, ?, 'ready', ?, ?, ?)`, scope.TenantID, scope.RepositoryID, id, timestamp(now), timestamp(delivery.CreatedAt), timestamp(now)); err != nil {
		return Delivery{}, fmt.Errorf("insert promoted delivery queue record: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Delivery{}, fmt.Errorf("commit delivery queue promotion: %w", err)
	}
	return delivery, nil
}

// Claim atomically selects one due queue record, fences its previous owner,
// records an attempt, and moves the delivery projection to running.
func (s *DeliveryStore) Claim(ctx context.Context, ownerID string, leaseDuration time.Duration) (Lease, error) {
	if ownerID == "" || leaseDuration <= 0 {
		return Lease{}, ErrInvalidDelivery
	}
	now := s.clock.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, fmt.Errorf("begin delivery claim: %w", err)
	}
	defer tx.Rollback()

	var scope DeliveryScope
	var id DeliveryID
	err = tx.QueryRowContext(ctx, `SELECT tenant_id, repository_id, delivery_id FROM delivery_queue
WHERE (status = 'ready' OR (status = 'waiting' AND waiting_signal = 'retry')) AND available_at <= ?
   OR status = 'leased' AND lease_expires_at <= ?
ORDER BY available_at, created_at, delivery_id LIMIT 1`, timestamp(now), timestamp(now)).Scan(&scope.TenantID, &scope.RepositoryID, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNoRunnableDelivery
	}
	if err != nil {
		return Lease{}, fmt.Errorf("select runnable delivery: %w", err)
	}

	var epoch int64
	var acquiredAt string
	expires := now.Add(leaseDuration)
	err = tx.QueryRowContext(ctx, `UPDATE delivery_queue SET status = 'leased', waiting_signal = '', lease_owner_id = ?,
lease_epoch = lease_epoch + 1, lease_acquired_at = ?, lease_expires_at = ?, lease_heartbeat_at = ?, updated_at = ?
WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?
RETURNING lease_epoch, lease_acquired_at`, ownerID, timestamp(now), timestamp(expires), timestamp(now), timestamp(now), scope.TenantID, scope.RepositoryID, id).Scan(&epoch, &acquiredAt)
	if err != nil {
		return Lease{}, fmt.Errorf("lease delivery: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_worker_attempts SET finished_at = ?, outcome = 'lease_expired', error_text = 'lease expired before completion' WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND finished_at IS NULL`, timestamp(now), scope.TenantID, scope.RepositoryID, id); err != nil {
		return Lease{}, fmt.Errorf("expire previous delivery attempt: %w", err)
	}
	delivery, err := loadDelivery(ctx, tx, scope, id)
	if err != nil {
		return Lease{}, err
	}
	if delivery.State == DeliveryWaitingRetry {
		if err := transitionInTx(ctx, tx, &delivery, DeliveryQueued, queueEventIdentity(id, "retry-due", epoch, now)); err != nil {
			return Lease{}, err
		}
	}
	if delivery.State != DeliveryRunning {
		if delivery.State != DeliveryQueued {
			return Lease{}, fmt.Errorf("claim delivery in state %s: %w", delivery.State, ErrInvalidDeliveryTransition)
		}
		if err := transitionInTx(ctx, tx, &delivery, DeliveryRunning, queueEventIdentity(id, "claim", epoch, now)); err != nil {
			return Lease{}, err
		}
	}
	var attempt int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt), 0) + 1 FROM delivery_worker_attempts WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?`, scope.TenantID, scope.RepositoryID, id).Scan(&attempt); err != nil {
		return Lease{}, fmt.Errorf("allocate delivery attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_worker_attempts (tenant_id, repository_id, delivery_id, attempt, owner_id, lease_epoch, started_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, scope.TenantID, scope.RepositoryID, id, attempt, ownerID, epoch, timestamp(now)); err != nil {
		return Lease{}, fmt.Errorf("record delivery attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, fmt.Errorf("commit delivery claim: %w", err)
	}
	return Lease{Delivery: delivery, OwnerID: ownerID, Epoch: epoch, Attempt: attempt, Acquired: now, Expires: expires, Heartbeat: now}, nil
}

func (s *DeliveryStore) Heartbeat(ctx context.Context, lease Lease, leaseDuration time.Duration) (Lease, error) {
	if leaseDuration <= 0 {
		return Lease{}, ErrInvalidDelivery
	}
	now := s.clock.Now().UTC()
	expires := now.Add(leaseDuration)
	result, err := s.db.ExecContext(ctx, `UPDATE delivery_queue SET lease_expires_at = ?, lease_heartbeat_at = ?, updated_at = ?
WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND status = 'leased' AND lease_owner_id = ? AND lease_epoch = ? AND lease_expires_at > ?`, timestamp(expires), timestamp(now), timestamp(now), lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, lease.OwnerID, lease.Epoch, timestamp(now))
	if err != nil {
		return Lease{}, fmt.Errorf("heartbeat delivery lease: %w", err)
	}
	if err := requireChanged(result); err != nil {
		return Lease{}, err
	}
	lease.Heartbeat, lease.Expires = now, expires
	return lease, nil
}

func (s *DeliveryStore) Checkpoint(ctx context.Context, lease Lease, checkpoint json.RawMessage) error {
	return s.updateAttempt(ctx, lease, "checkpoint_json", checkpoint)
}

func (s *DeliveryStore) PublishResult(ctx context.Context, lease Lease, result json.RawMessage) error {
	return s.updateAttempt(ctx, lease, "result_json", result)
}

func (s *DeliveryStore) updateAttempt(ctx context.Context, lease Lease, column string, value json.RawMessage) error {
	now := s.clock.Now().UTC()
	query := `UPDATE delivery_worker_attempts SET ` + column + ` = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND attempt = ? AND owner_id = ? AND lease_epoch = ?
AND EXISTS (SELECT 1 FROM delivery_queue WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND status = 'leased' AND lease_owner_id = ? AND lease_epoch = ? AND lease_expires_at > ?)`
	result, err := s.db.ExecContext(ctx, query, string(value), lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, lease.Attempt, lease.OwnerID, lease.Epoch, lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, lease.OwnerID, lease.Epoch, timestamp(now))
	if err != nil {
		return fmt.Errorf("update delivery attempt: %w", err)
	}
	return requireChanged(result)
}

// TransitionLease changes delivery state only while the caller owns the live
// fencing token. Queue disposition remains unchanged.
func (s *DeliveryStore) TransitionLease(ctx context.Context, lease Lease, next DeliveryState) (Delivery, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, fmt.Errorf("begin leased delivery transition: %w", err)
	}
	defer tx.Rollback()
	if err := verifyLease(ctx, tx, lease, s.clock.Now().UTC()); err != nil {
		return Delivery{}, err
	}
	delivery, err := loadDelivery(ctx, tx, lease.Delivery.DeliveryScope, lease.Delivery.ID)
	if err != nil {
		return Delivery{}, err
	}
	if err := transitionInTx(ctx, tx, &delivery, next, queueEventIdentity(delivery.ID, "transition-"+string(next), lease.Epoch, s.clock.Now().UTC())); err != nil {
		return Delivery{}, err
	}
	if err := tx.Commit(); err != nil {
		return Delivery{}, fmt.Errorf("commit leased delivery transition: %w", err)
	}
	return delivery, nil
}

// Release explicitly yields work back to the runnable queue.
func (s *DeliveryStore) Release(ctx context.Context, lease Lease) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delivery release: %w", err)
	}
	defer tx.Rollback()
	now := s.clock.Now().UTC()
	if err := verifyLease(ctx, tx, lease, now); err != nil {
		return err
	}
	delivery, err := loadDelivery(ctx, tx, lease.Delivery.DeliveryScope, lease.Delivery.ID)
	if err != nil {
		return err
	}
	if delivery.State != DeliveryQueued {
		if err := transitionInTx(ctx, tx, &delivery, DeliveryQueued, queueEventIdentity(delivery.ID, "release", lease.Epoch, now)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status = 'ready', available_at = ?, lease_owner_id = NULL, lease_acquired_at = NULL, lease_expires_at = NULL, lease_heartbeat_at = NULL, updated_at = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?`, timestamp(now), timestamp(now), delivery.TenantID, delivery.RepositoryID, delivery.ID); err != nil {
		return fmt.Errorf("release delivery: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delivery release: %w", err)
	}
	return nil
}

// Finalize is the fenced final write for one worker attempt.
func (s *DeliveryStore) Finalize(ctx context.Context, lease Lease, outcome Outcome) (Delivery, error) {
	if outcome.ActiveNanoseconds < 0 {
		return Delivery{}, ErrInvalidDelivery
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, fmt.Errorf("begin delivery outcome: %w", err)
	}
	defer tx.Rollback()
	now := s.clock.Now().UTC()
	if err := verifyLease(ctx, tx, lease, now); err != nil {
		return Delivery{}, err
	}
	delivery, err := loadDelivery(ctx, tx, lease.Delivery.DeliveryScope, lease.Delivery.ID)
	if err != nil {
		return Delivery{}, err
	}
	next, queueStatus, availableAt, signal, err := validateOutcome(outcome, now)
	if err != nil {
		return Delivery{}, err
	}
	if delivery.State != next {
		if err := transitionInTx(ctx, tx, &delivery, next, queueEventIdentity(delivery.ID, "outcome-"+string(outcome.Kind), lease.Epoch, now)); err != nil {
			return Delivery{}, err
		}
	}
	errorText := ""
	if outcome.Err != nil {
		errorText = outcome.Err.Error()
	}
	resultJSON := string(outcome.Result)
	attemptResult, err := tx.ExecContext(ctx, `UPDATE delivery_worker_attempts SET finished_at = ?, outcome = ?, result_json = CASE WHEN ? = '' THEN result_json ELSE ? END, error_text = ?, active_nanoseconds = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND attempt = ? AND owner_id = ? AND lease_epoch = ?`, timestamp(now), outcome.Kind, resultJSON, resultJSON, errorText, outcome.ActiveNanoseconds, delivery.TenantID, delivery.RepositoryID, delivery.ID, lease.Attempt, lease.OwnerID, lease.Epoch)
	if err != nil {
		return Delivery{}, fmt.Errorf("finish delivery attempt: %w", err)
	}
	if err := requireChanged(attemptResult); err != nil {
		return Delivery{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status = ?, available_at = ?, waiting_signal = ?, result_json = CASE WHEN ? = '' THEN result_json ELSE ? END, lease_owner_id = NULL, lease_acquired_at = NULL, lease_expires_at = NULL, lease_heartbeat_at = NULL, updated_at = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?`, queueStatus, timestamp(availableAt), signal, resultJSON, resultJSON, timestamp(now), delivery.TenantID, delivery.RepositoryID, delivery.ID); err != nil {
		return Delivery{}, fmt.Errorf("finish delivery queue record: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Delivery{}, fmt.Errorf("commit delivery outcome: %w", err)
	}
	return delivery, nil
}

func validateOutcome(outcome Outcome, now time.Time) (DeliveryState, string, time.Time, string, error) {
	switch outcome.Kind {
	case OutcomeSucceeded:
		return DeliverySucceeded, "done", now, "", nil
	case OutcomeFailed:
		return DeliveryFailed, "done", now, "", nil
	case OutcomeCancelled:
		return DeliveryCancelled, "cancelled", now, "", nil
	case OutcomeRetry:
		if outcome.RetryAt.IsZero() || outcome.RetryAt.Before(now) {
			return "", "", time.Time{}, "", ErrInvalidDelivery
		}
		return DeliveryWaitingRetry, "waiting", outcome.RetryAt.UTC(), "retry", nil
	case OutcomeWaiting:
		if outcome.Signal == "" || !stateIn(outcome.WaitState, DeliveryWaitingDecision, DeliveryWaitingCredentials, DeliveryWaitingQuota, DeliveryPaused) {
			return "", "", time.Time{}, "", ErrInvalidDelivery
		}
		return outcome.WaitState, "waiting", now, outcome.Signal, nil
	default:
		return "", "", time.Time{}, "", ErrInvalidDelivery
	}
}

// SignalWaiting records the external signal by moving a non-retry wait back to
// the runnable queue. A mismatched signal cannot wake the delivery.
func (s *DeliveryStore) SignalWaiting(ctx context.Context, scope DeliveryScope, id DeliveryID, signal string) error {
	if err := validateDeliveryRef(scope, id); err != nil || signal == "" || signal == "retry" {
		return ErrInvalidDelivery
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delivery signal: %w", err)
	}
	defer tx.Rollback()
	now := s.clock.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status = 'ready', available_at = ?, waiting_signal = '', updated_at = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND status = 'waiting' AND waiting_signal = ?`, timestamp(now), timestamp(now), scope.TenantID, scope.RepositoryID, id, signal)
	if err != nil {
		return fmt.Errorf("signal waiting delivery: %w", err)
	}
	if err := requireChanged(result); err != nil {
		return err
	}
	delivery, err := loadDelivery(ctx, tx, scope, id)
	if err != nil {
		return err
	}
	if err := transitionInTx(ctx, tx, &delivery, DeliveryQueued, queueEventIdentity(id, "signal-"+signal, delivery.Version, now)); err != nil {
		return err
	}
	return tx.Commit()
}

// Cancel records the explicit durable cancellation signal and fences any
// current owner so no further lease-sensitive write can succeed.
func (s *DeliveryStore) Cancel(ctx context.Context, scope DeliveryScope, id DeliveryID) error {
	if err := validateDeliveryRef(scope, id); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delivery cancellation: %w", err)
	}
	defer tx.Rollback()
	delivery, err := loadDelivery(ctx, tx, scope, id)
	if err != nil {
		return err
	}
	if delivery.State == DeliveryCancelRequested || delivery.State == DeliveryCancelled {
		return nil
	}
	if isTerminalDeliveryState(delivery.State) {
		return ErrDeliveryCancelled
	}
	now := s.clock.Now().UTC()
	if err := transitionInTx(ctx, tx, &delivery, DeliveryCancelRequested, queueEventIdentity(id, "cancel", delivery.Version, now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_worker_attempts SET finished_at = ?, outcome = 'cancelled', error_text = 'explicit cancellation' WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND finished_at IS NULL`, timestamp(now), scope.TenantID, scope.RepositoryID, id); err != nil {
		return fmt.Errorf("cancel delivery attempt: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status = 'cancelled', waiting_signal = 'cancel', lease_epoch = lease_epoch + 1, lease_owner_id = NULL, lease_acquired_at = NULL, lease_expires_at = NULL, lease_heartbeat_at = NULL, updated_at = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?`, timestamp(now), scope.TenantID, scope.RepositoryID, id)
	if err != nil {
		return fmt.Errorf("cancel delivery queue record: %w", err)
	}
	if err := requireChanged(result); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delivery cancellation: %w", err)
	}
	return nil
}

func (s *DeliveryStore) Attempts(ctx context.Context, scope DeliveryScope, id DeliveryID) ([]AttemptRecord, error) {
	if err := validateDeliveryRef(scope, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT attempt, owner_id, lease_epoch, started_at, finished_at, outcome, checkpoint_json, result_json, error_text FROM delivery_worker_attempts WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? ORDER BY attempt`, scope.TenantID, scope.RepositoryID, id)
	if err != nil {
		return nil, fmt.Errorf("list delivery attempts: %w", err)
	}
	defer rows.Close()
	var attempts []AttemptRecord
	for rows.Next() {
		var record AttemptRecord
		var started string
		var finished sql.NullString
		var checkpoint, result string
		if err := rows.Scan(&record.Attempt, &record.OwnerID, &record.LeaseEpoch, &started, &finished, &record.Outcome, &checkpoint, &result, &record.Error); err != nil {
			return nil, fmt.Errorf("scan delivery attempt: %w", err)
		}
		record.StartedAt, err = parseTimestamp(started)
		if err != nil {
			return nil, err
		}
		if finished.Valid {
			value, err := parseTimestamp(finished.String)
			if err != nil {
				return nil, err
			}
			record.FinishedAt = &value
		}
		record.Checkpoint, record.Result = json.RawMessage(checkpoint), json.RawMessage(result)
		attempts = append(attempts, record)
	}
	return attempts, rows.Err()
}

func verifyLease(ctx context.Context, tx *sql.Tx, lease Lease, now time.Time) error {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM delivery_queue WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND status = 'leased' AND lease_owner_id = ? AND lease_epoch = ? AND lease_expires_at > ?`, lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, lease.OwnerID, lease.Epoch, timestamp(now)).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleLease
	}
	if err != nil {
		return fmt.Errorf("verify delivery lease: %w", err)
	}
	return nil
}

func transitionInTx(ctx context.Context, tx *sql.Tx, delivery *Delivery, next DeliveryState, identity EventIdentity) error {
	from := delivery.State
	if err := delivery.Transition(next); err != nil {
		return err
	}
	delivery.Version++
	delivery.UpdatedAt = identity.OccurredAt
	payload, err := json.Marshal(versionedTransitionedPayload{transitionedPayload: transitionedPayload{From: from, To: next}, Version: delivery.Version})
	if err != nil {
		return fmt.Errorf("encode delivery transition: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_deliveries SET state = ?, version = ?, updated_at = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?`, delivery.State, delivery.Version, timestamp(delivery.UpdatedAt), delivery.TenantID, delivery.RepositoryID, delivery.ID); err != nil {
		return fmt.Errorf("update delivery transition: %w", err)
	}
	_, err = appendDeliveryEvent(ctx, tx, delivery.DeliveryScope, delivery.ID, DeliveryEventTransitioned, payload, identity, "")
	return err
}

func queueEventIdentity(id DeliveryID, action string, epoch int64, at time.Time) EventIdentity {
	key := fmt.Sprintf("queue:%s:%s:%d", id, action, epoch)
	return EventIdentity{ID: DeliveryEventID(key), IdempotencyKey: DeliveryIdempotencyKey(key), OccurredAt: at}
}

func requireChanged(result sql.Result) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count delivery queue update: %w", err)
	}
	if changed != 1 {
		return ErrStaleLease
	}
	return nil
}
