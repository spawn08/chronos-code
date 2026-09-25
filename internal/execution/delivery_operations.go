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
	ErrOperationConflict         = errors.New("operation identity conflict")
	ErrOperationNotFound         = errors.New("operation not found")
	ErrEffectNeedsReconciliation = errors.New("operation effect outcome requires reconciliation")
)

type ReplayClass string

const (
	ReplayRead               ReplayClass = "read"
	ReplayFingerprintedWrite ReplayClass = "fingerprinted_write"
	ReplayIdempotentExternal ReplayClass = "idempotent_external"
	ReplayUnknown            ReplayClass = "unknown"
)

type OperationStatus string

const (
	OperationPrepared   OperationStatus = "prepared"
	OperationRunning    OperationStatus = "running"
	OperationObserved   OperationStatus = "observed"
	OperationReconciled OperationStatus = "reconciled"
)

type Operation struct {
	ID                        string
	EffectKey                 string
	Kind                      string
	ReplayClass               ReplayClass
	InputFingerprint          string
	ArgumentsFingerprint      string
	ObservationPath           string
	InputStateFingerprint     string
	ExpectedOutputFingerprint string
	OutputFingerprint         string
	GoalRevision              GoalRevision
	Status                    OperationStatus
	OwnerID                   string
	LeaseEpoch                int64
	Attempt                   int64
	PreparedAt                time.Time
	UpdatedAt                 time.Time
	Result                    json.RawMessage
	Error                     string
}

// PrepareOperation durably records intent under a live lease before any effect.
// A prepared record is safe to resume; a running record has an ambiguous
// outcome and may not be executed again without explicit reconciliation.
func (s *DeliveryStore) PrepareOperation(ctx context.Context, lease Lease, op Operation) (Operation, error) {
	if op.ID == "" || op.EffectKey == "" || op.Kind == "" || op.InputFingerprint == "" ||
		!stateInReplay(op.ReplayClass) {
		return Operation{}, ErrInvalidDelivery
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, fmt.Errorf("begin operation prepare: %w", err)
	}
	defer tx.Rollback()
	if err := verifyLease(ctx, tx, lease, s.clock.Now().UTC()); err != nil {
		return Operation{}, err
	}
	var revision GoalRevision
	if err := tx.QueryRowContext(ctx, `SELECT current_goal_revision FROM delivery_deliveries WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?`, lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID).Scan(&revision); err != nil {
		return Operation{}, fmt.Errorf("load operation goal revision: %w", err)
	}
	if revision != lease.Delivery.CurrentGoalRevision {
		return Operation{}, ErrStaleDeliveryVersion
	}
	existing, err := loadOperation(ctx, tx, lease, op.ID)
	if err == nil {
		if existing.EffectKey != op.EffectKey || existing.Kind != op.Kind || existing.ReplayClass != op.ReplayClass || existing.GoalRevision != revision ||
			existing.ArgumentsFingerprint != op.ArgumentsFingerprint || existing.ObservationPath != op.ObservationPath || existing.ExpectedOutputFingerprint != op.ExpectedOutputFingerprint {
			return Operation{}, ErrOperationConflict
		}
		if existing.InputFingerprint != op.InputFingerprint && !(existing.ReplayClass == ReplayFingerprintedWrite &&
			(existing.Status == OperationObserved || existing.Status == OperationReconciled) &&
			existing.ExpectedOutputFingerprint != "" && existing.OutputFingerprint == existing.ExpectedOutputFingerprint &&
			op.InputStateFingerprint == existing.ExpectedOutputFingerprint) {
			return Operation{}, ErrOperationConflict
		}
		if existing.Status == OperationRunning {
			return existing, ErrEffectNeedsReconciliation
		}
		return existing, nil
	}
	if !errors.Is(err, ErrOperationNotFound) {
		return Operation{}, err
	}
	var existingID string
	if err := tx.QueryRowContext(ctx, `SELECT operation_id FROM delivery_operations WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND effect_key = ?`, lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, op.EffectKey).Scan(&existingID); err == nil {
		return Operation{}, ErrOperationConflict
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Operation{}, fmt.Errorf("check operation effect key: %w", err)
	}
	now := s.clock.Now().UTC()
	op.Status, op.OwnerID, op.LeaseEpoch, op.Attempt, op.GoalRevision, op.PreparedAt, op.UpdatedAt = OperationPrepared, lease.OwnerID, lease.Epoch, lease.Attempt, revision, now, now
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_operations (tenant_id, repository_id, delivery_id, operation_id, effect_key, kind, replay_class, input_fingerprint, arguments_fingerprint, observation_path, input_state_fingerprint, expected_output_fingerprint, goal_revision, status, owner_id, lease_epoch, attempt, prepared_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, op.ID, op.EffectKey, op.Kind, op.ReplayClass, op.InputFingerprint, op.ArgumentsFingerprint, op.ObservationPath, op.InputStateFingerprint, op.ExpectedOutputFingerprint, op.GoalRevision, op.Status, op.OwnerID, op.LeaseEpoch, op.Attempt, timestamp(now), timestamp(now)); err != nil {
		return Operation{}, fmt.Errorf("persist prepared operation: %w", err)
	}
	if err := appendOperationEvent(ctx, tx, lease, op, DeliveryEventOperationPrepared); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Operation{}, fmt.Errorf("commit prepared operation: %w", err)
	}
	return op, nil
}

// BeginOperation must commit immediately before invoking the effect. Recovery
// treats a running record as unknown even when the process died before the
// effect actually started.
func (s *DeliveryStore) BeginOperation(ctx context.Context, lease Lease, id string) (Operation, error) {
	return s.advanceOperation(ctx, lease, id, OperationPrepared, OperationRunning, "", nil, "")
}

// ObserveOperation persists the observed result; on storage failure the
// running record remains ambiguous and execution must stop rather than replay.
func (s *DeliveryStore) ObserveOperation(ctx context.Context, lease Lease, id string, result json.RawMessage, outputFingerprint, errorText string) (Operation, error) {
	if len(result) > 0 && !json.Valid(result) {
		return Operation{}, ErrInvalidDelivery
	}
	return s.advanceOperation(ctx, lease, id, OperationRunning, OperationObserved, outputFingerprint, result, errorText)
}

// ReconcileOperation records host-observed proof for an ambiguous effect.
// A model cannot call this through the tool registry. Proof must identify the
// check that established the outcome; it never implicitly retries a shell.
func (s *DeliveryStore) ReconcileOperation(ctx context.Context, lease Lease, id, proof, outputFingerprint string, result json.RawMessage) (Operation, error) {
	if proof == "" || (len(result) > 0 && !json.Valid(result)) {
		return Operation{}, ErrInvalidDelivery
	}
	return s.advanceOperation(ctx, lease, id, OperationRunning, OperationReconciled, outputFingerprint, result, proof)
}

func (s *DeliveryStore) advanceOperation(ctx context.Context, lease Lease, id string, from, to OperationStatus, outputFingerprint string, result json.RawMessage, errorText string) (Operation, error) {
	if id == "" {
		return Operation{}, ErrInvalidDelivery
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, fmt.Errorf("begin operation transition: %w", err)
	}
	defer tx.Rollback()
	now := s.clock.Now().UTC()
	if err := verifyLease(ctx, tx, lease, now); err != nil {
		return Operation{}, err
	}
	var revision GoalRevision
	if err := tx.QueryRowContext(ctx, `SELECT current_goal_revision FROM delivery_deliveries WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?`, lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID).Scan(&revision); err != nil {
		return Operation{}, fmt.Errorf("load operation goal revision: %w", err)
	}
	op, err := loadOperation(ctx, tx, lease, id)
	if err != nil {
		return Operation{}, err
	}
	if op.GoalRevision != lease.Delivery.CurrentGoalRevision || op.GoalRevision != revision {
		return Operation{}, ErrStaleDeliveryVersion
	}
	if op.Status != from {
		if op.Status == OperationRunning {
			return Operation{}, ErrEffectNeedsReconciliation
		}
		return Operation{}, ErrInvalidDeliveryTransition
	}
	if from == OperationRunning && to != OperationReconciled && (op.OwnerID != lease.OwnerID || op.LeaseEpoch != lease.Epoch || op.Attempt != lease.Attempt) {
		return Operation{}, ErrEffectNeedsReconciliation
	}
	op.Status, op.OwnerID, op.LeaseEpoch, op.Attempt, op.UpdatedAt = to, lease.OwnerID, lease.Epoch, lease.Attempt, now
	op.OutputFingerprint, op.Result, op.Error = outputFingerprint, result, errorText
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_operations SET status = ?, owner_id = ?, lease_epoch = ?, attempt = ?, updated_at = ?, output_fingerprint = ?, result_json = ?, error_text = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND operation_id = ?`, op.Status, op.OwnerID, op.LeaseEpoch, op.Attempt, timestamp(now), op.OutputFingerprint, string(op.Result), op.Error, lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, op.ID); err != nil {
		return Operation{}, fmt.Errorf("persist operation transition: %w", err)
	}
	eventType := DeliveryEventOperationRunning
	if to == OperationObserved {
		eventType = DeliveryEventOperationObserved
	} else if to == OperationReconciled {
		eventType = DeliveryEventOperationReconciled
	}
	if err := appendOperationEvent(ctx, tx, lease, op, eventType); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Operation{}, fmt.Errorf("commit operation transition: %w", err)
	}
	return op, nil
}

func (s *DeliveryStore) Operation(ctx context.Context, scope DeliveryScope, deliveryID DeliveryID, operationID string) (Operation, error) {
	if err := validateDeliveryRef(scope, deliveryID); err != nil || operationID == "" {
		return Operation{}, ErrInvalidDelivery
	}
	return loadOperation(ctx, s.db, Lease{Delivery: Delivery{DeliveryScope: scope, ID: deliveryID}}, operationID)
}

// Operations enumerates authoritative effect records before resuming a new
// attempt. Prior-attempt effects cannot be silently replayed with new model IDs.
func (s *DeliveryStore) Operations(ctx context.Context, scope DeliveryScope, deliveryID DeliveryID) ([]Operation, error) {
	if err := validateDeliveryRef(scope, deliveryID); err != nil {
		return nil, err
	}
	if _, err := s.Load(ctx, scope, deliveryID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT operation_id FROM delivery_operations WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? ORDER BY prepared_at, operation_id`, scope.TenantID, scope.RepositoryID, deliveryID)
	if err != nil {
		return nil, fmt.Errorf("list delivery operations: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan delivery operation ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list delivery operation IDs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close delivery operation IDs: %w", err)
	}
	operations := make([]Operation, 0, len(ids))
	for _, id := range ids {
		op, err := s.Operation(ctx, scope, deliveryID, id)
		if err != nil {
			return nil, err
		}
		operations = append(operations, op)
	}
	return operations, nil
}

func loadOperation(ctx context.Context, db queryer, lease Lease, id string) (Operation, error) {
	var op Operation
	var prepared, updated, result string
	err := db.QueryRowContext(ctx, `SELECT operation_id, effect_key, kind, replay_class, input_fingerprint, arguments_fingerprint, observation_path, input_state_fingerprint, expected_output_fingerprint, output_fingerprint, goal_revision, status, owner_id, lease_epoch, attempt, prepared_at, updated_at, result_json, error_text FROM delivery_operations WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND operation_id = ?`, lease.Delivery.TenantID, lease.Delivery.RepositoryID, lease.Delivery.ID, id).Scan(&op.ID, &op.EffectKey, &op.Kind, &op.ReplayClass, &op.InputFingerprint, &op.ArgumentsFingerprint, &op.ObservationPath, &op.InputStateFingerprint, &op.ExpectedOutputFingerprint, &op.OutputFingerprint, &op.GoalRevision, &op.Status, &op.OwnerID, &op.LeaseEpoch, &op.Attempt, &prepared, &updated, &result, &op.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrOperationNotFound
	}
	if err != nil {
		return Operation{}, fmt.Errorf("load delivery operation: %w", err)
	}
	if op.PreparedAt, err = parseTimestamp(prepared); err != nil {
		return Operation{}, err
	}
	if op.UpdatedAt, err = parseTimestamp(updated); err != nil {
		return Operation{}, err
	}
	op.Result = json.RawMessage(result)
	return op, nil
}

func appendOperationEvent(ctx context.Context, tx *sql.Tx, lease Lease, op Operation, kind DeliveryEventType) error {
	payload, err := json.Marshal(struct {
		OperationID string          `json:"operation_id"`
		EffectKey   string          `json:"effect_key"`
		Status      OperationStatus `json:"status"`
		Attempt     int64           `json:"attempt"`
		LeaseEpoch  int64           `json:"lease_epoch"`
	}{op.ID, op.EffectKey, op.Status, op.Attempt, op.LeaseEpoch})
	if err != nil {
		return fmt.Errorf("encode operation event: %w", err)
	}
	key := fmt.Sprintf("operation:%s:%s:%d", op.ID, kind, op.Attempt)
	_, err = appendDeliveryEvent(ctx, tx, lease.Delivery.DeliveryScope, lease.Delivery.ID, kind, payload, EventIdentity{ID: DeliveryEventID(key), IdempotencyKey: DeliveryIdempotencyKey(key), OccurredAt: op.UpdatedAt}, "")
	return err
}

func stateInReplay(class ReplayClass) bool {
	return class == ReplayRead || class == ReplayFingerprintedWrite || class == ReplayIdempotentExternal || class == ReplayUnknown
}
