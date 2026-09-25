package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const deliverySchemaVersion = 8

var deliveryMigrations = []struct {
	version  int
	checksum string
	sql      string
}{
	{version: 1, checksum: deliverySchemaChecksum(deliverySchemaV1), sql: deliverySchemaV1},
	{version: 2, checksum: deliverySchemaChecksum(deliverySchemaV2), sql: deliverySchemaV2},
	{version: 3, checksum: deliverySchemaChecksum(deliverySchemaV3), sql: deliverySchemaV3},
	{version: 4, checksum: deliverySchemaChecksum(deliverySchemaV4), sql: deliverySchemaV4},
	{version: 5, checksum: deliverySchemaChecksum(deliverySchemaV5), sql: deliverySchemaV5},
	{version: 6, checksum: deliverySchemaChecksum(deliverySchemaV6), sql: deliverySchemaV6},
	{version: 7, checksum: deliverySchemaChecksum(deliverySchemaV7), sql: deliverySchemaV7},
	{version: 8, checksum: deliverySchemaChecksum(deliverySchemaV8), sql: deliverySchemaV8},
}

// DeliveryStore is the SQLite-backed durable delivery repository.
type DeliveryStore struct {
	db    *sql.DB
	clock Clock
	path  string
}

func OpenDeliveryStore(ctx context.Context, path string) (*DeliveryStore, error) {
	return OpenDeliveryStoreWithClock(ctx, path, systemClock{})
}

func OpenDeliveryStoreWithClock(ctx context.Context, path string, clock Clock) (*DeliveryStore, error) {
	if clock == nil {
		return nil, ErrInvalidDelivery
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open delivery store: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &DeliveryStore{db: db, clock: clock, path: path}
	if err := store.refuseNewerSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure delivery store: %w", err)
	}
	if err := store.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *DeliveryStore) Close() error { return s.db.Close() }

// Migrate applies schema and checksum markers in the same transaction.
func (s *DeliveryStore) Migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delivery migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS delivery_schema_migrations (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create delivery migration table: %w", err)
	}
	var latest int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM delivery_schema_migrations`).Scan(&latest); err != nil {
		return fmt.Errorf("read latest delivery migration: %w", err)
	}
	if latest > deliverySchemaVersion {
		return ErrUnsupportedDeliverySchema
	}
	for _, migration := range deliveryMigrations {
		var checksum string
		err := tx.QueryRowContext(ctx, `SELECT checksum FROM delivery_schema_migrations WHERE version = ?`, migration.version).Scan(&checksum)
		if err == nil {
			if checksum != migration.checksum {
				return ErrIncompatibleDeliverySchema
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read delivery migration %d: %w", migration.version, err)
		}
		if migration.version <= latest {
			return ErrIncompatibleDeliverySchema
		}
		if _, err := tx.ExecContext(ctx, migration.sql); err != nil {
			return fmt.Errorf("apply delivery migration %d: %w", migration.version, err)
		}
		if migration.version == 2 {
			if err := backfillDeliveryMutationFingerprints(ctx, tx); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_schema_migrations (version, checksum) VALUES (?, ?)`, migration.version, migration.checksum); err != nil {
			return fmt.Errorf("record delivery migration %d: %w", migration.version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delivery migration: %w", err)
	}
	return nil
}

func (s *DeliveryStore) refuseNewerSchema(ctx context.Context) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'delivery_schema_migrations')`).Scan(&exists); err != nil {
		return fmt.Errorf("inspect delivery schema: %w", err)
	}
	if exists == 0 {
		return nil
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM delivery_schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("read delivery schema version: %w", err)
	}
	if version > deliverySchemaVersion {
		return ErrUnsupportedDeliverySchema
	}
	return nil
}

// Admit atomically persists the initial aggregate and admission event. An
// identical retry returns the first result; reuse with different content fails.
func (s *DeliveryStore) Admit(ctx context.Context, admission Admission) (Delivery, error) {
	return s.admit(ctx, admission, false)
}

// AdmitRunnable persists admission, queued delivery state, and its runnable
// outbox record in one transaction. Repeating the same admission never creates
// another queue record.
func (s *DeliveryStore) AdmitRunnable(ctx context.Context, admission Admission) (Delivery, error) {
	return s.admit(ctx, admission, true)
}

func (s *DeliveryStore) admit(ctx context.Context, admission Admission, runnable bool) (Delivery, error) {
	normalized, fingerprint, err := normalizeAdmission(admission)
	if err != nil {
		return Delivery{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, fmt.Errorf("begin delivery admission: %w", err)
	}
	defer tx.Rollback()

	var existingID DeliveryID
	var existingFingerprint string
	err = tx.QueryRowContext(ctx, `SELECT delivery_id, admission_fingerprint FROM delivery_deliveries WHERE tenant_id = ? AND repository_id = ? AND admission_key = ?`, normalized.Scope.TenantID, normalized.Scope.RepositoryID, normalized.AdmissionKey).Scan(&existingID, &existingFingerprint)
	if err == nil {
		if existingID != normalized.DeliveryID || existingFingerprint != fingerprint {
			return Delivery{}, ErrAdmissionConflict
		}
		delivery, err := loadDelivery(ctx, tx, normalized.Scope, existingID)
		if err != nil || !runnable {
			return delivery, err
		}
		if delivery.State == DeliveryAdmitted {
			if err := transitionInTx(ctx, tx, &delivery, DeliveryQueued, queueEventIdentity(delivery.ID, "admit", 0, normalized.Event.OccurredAt)); err != nil {
				return Delivery{}, err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_queue (tenant_id, repository_id, delivery_id, status, available_at, created_at, updated_at) VALUES (?, ?, ?, 'ready', ?, ?, ?) ON CONFLICT (tenant_id, repository_id, delivery_id) DO NOTHING`, delivery.TenantID, delivery.RepositoryID, delivery.ID, timestamp(delivery.CreatedAt), timestamp(delivery.CreatedAt), timestamp(delivery.CreatedAt)); err != nil {
			return Delivery{}, fmt.Errorf("ensure delivery queue record: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return Delivery{}, fmt.Errorf("commit duplicate delivery admission: %w", err)
		}
		return delivery, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, fmt.Errorf("check duplicate delivery admission: %w", err)
	}
	var existingAdmissionKey AdmissionKey
	err = tx.QueryRowContext(ctx, `SELECT admission_key FROM delivery_deliveries WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?`, normalized.Scope.TenantID, normalized.Scope.RepositoryID, normalized.DeliveryID).Scan(&existingAdmissionKey)
	if err == nil {
		return Delivery{}, ErrAdmissionConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, fmt.Errorf("check duplicate delivery ID: %w", err)
	}

	delivery := Delivery{
		DeliveryScope:       normalized.Scope,
		ID:                  normalized.DeliveryID,
		AdmissionKey:        normalized.AdmissionKey,
		State:               DeliveryAdmitted,
		Version:             1,
		CurrentGoalRevision: 1,
		PolicyReference:     normalized.PolicyReference,
		MaxCostMicrodollars: normalized.MaxCostMicrodollars,
		CreatedAt:           normalized.Event.OccurredAt,
		UpdatedAt:           normalized.Event.OccurredAt,
		Goals:               []Goal{normalized.Goal},
		Requirements:        normalized.Requirements,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_deliveries (tenant_id, repository_id, delivery_id, admission_key, admission_fingerprint, state, version, current_goal_revision, policy_reference, max_cost_microdollars, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, delivery.TenantID, delivery.RepositoryID, delivery.ID, delivery.AdmissionKey, fingerprint, delivery.State, delivery.Version, delivery.CurrentGoalRevision, delivery.PolicyReference, delivery.MaxCostMicrodollars, timestamp(delivery.CreatedAt), timestamp(delivery.UpdatedAt)); err != nil {
		return Delivery{}, fmt.Errorf("insert delivery: %w", err)
	}
	if err := insertGoal(ctx, tx, delivery, normalized.Goal, normalized.Requirements); err != nil {
		return Delivery{}, err
	}
	payload, err := json.Marshal(admittedPayload{Delivery: delivery})
	if err != nil {
		return Delivery{}, fmt.Errorf("encode delivery admission event: %w", err)
	}
	if _, err := appendDeliveryEvent(ctx, tx, normalized.Scope, normalized.DeliveryID, DeliveryEventAdmitted, payload, normalized.Event, ""); err != nil {
		return Delivery{}, err
	}
	if runnable {
		from := delivery.State
		delivery.State = DeliveryQueued
		delivery.Version++
		transitionPayload, err := json.Marshal(versionedTransitionedPayload{transitionedPayload: transitionedPayload{From: from, To: delivery.State}, Version: delivery.Version})
		if err != nil {
			return Delivery{}, fmt.Errorf("encode queued delivery event: %w", err)
		}
		identity := queueEventIdentity(delivery.ID, "admit", 0, normalized.Event.OccurredAt)
		if _, err := appendDeliveryEvent(ctx, tx, normalized.Scope, normalized.DeliveryID, DeliveryEventTransitioned, transitionPayload, identity, ""); err != nil {
			return Delivery{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_deliveries SET state = ?, version = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?`, delivery.State, delivery.Version, delivery.TenantID, delivery.RepositoryID, delivery.ID); err != nil {
			return Delivery{}, fmt.Errorf("queue admitted delivery: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_queue (tenant_id, repository_id, delivery_id, status, available_at, created_at, updated_at) VALUES (?, ?, ?, 'ready', ?, ?, ?)`, delivery.TenantID, delivery.RepositoryID, delivery.ID, timestamp(delivery.CreatedAt), timestamp(delivery.CreatedAt), timestamp(delivery.CreatedAt)); err != nil {
			return Delivery{}, fmt.Errorf("insert delivery queue record: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Delivery{}, fmt.Errorf("commit delivery admission: %w", err)
	}
	return delivery, nil
}

func (s *DeliveryStore) Load(ctx context.Context, scope DeliveryScope, id DeliveryID) (Delivery, error) {
	if err := validateDeliveryRef(scope, id); err != nil {
		return Delivery{}, err
	}
	return loadDelivery(ctx, s.db, scope, id)
}

func (s *DeliveryStore) List(ctx context.Context, scope DeliveryScope) ([]DeliverySummary, error) {
	return s.listByStates(ctx, scope, nil)
}

// ListRunnable returns records that a future scheduler may act on. It does not
// lease or execute them.
func (s *DeliveryStore) ListRunnable(ctx context.Context, scope DeliveryScope) ([]DeliverySummary, error) {
	return s.listByStates(ctx, scope, []DeliveryState{DeliveryAdmitted, DeliveryQueued, DeliveryReconciling, DeliveryReplanning, DeliveryCancelRequested})
}

func (s *DeliveryStore) ListWaiting(ctx context.Context, scope DeliveryScope) ([]DeliverySummary, error) {
	return s.listByStates(ctx, scope, []DeliveryState{DeliveryWaitingRetry, DeliveryWaitingDecision, DeliveryWaitingCredentials, DeliveryWaitingQuota, DeliveryPaused})
}

func (s *DeliveryStore) listByStates(ctx context.Context, scope DeliveryScope, states []DeliveryState) ([]DeliverySummary, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT delivery_id, state, version, current_goal_revision, created_at, updated_at FROM delivery_deliveries WHERE tenant_id = ? AND repository_id = ?`
	args := []any{scope.TenantID, scope.RepositoryID}
	if len(states) > 0 {
		query += ` AND state IN (`
		for i, state := range states {
			if i > 0 {
				query += `,`
			}
			query += `?`
			args = append(args, state)
		}
		query += `)`
	}
	query += ` ORDER BY created_at, delivery_id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	defer rows.Close()
	var summaries []DeliverySummary
	for rows.Next() {
		var summary DeliverySummary
		var createdAt, updatedAt string
		if err := rows.Scan(&summary.ID, &summary.State, &summary.Version, &summary.CurrentGoalRevision, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan delivery summary: %w", err)
		}
		if summary.CreatedAt, err = parseTimestamp(createdAt); err != nil {
			return nil, err
		}
		if summary.UpdatedAt, err = parseTimestamp(updatedAt); err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	return summaries, nil
}

func (s *DeliveryStore) Transition(ctx context.Context, scope DeliveryScope, id DeliveryID, expectedVersion int64, next DeliveryState, event EventIdentity) (Delivery, error) {
	fingerprint, err := deliveryMutationFingerprint(DeliveryEventTransitioned, expectedVersion, transitionedPayload{To: next})
	if err != nil {
		return Delivery{}, err
	}
	return s.mutate(ctx, scope, id, expectedVersion, DeliveryEventTransitioned, event, fingerprint, func(ctx context.Context, tx *sql.Tx, delivery *Delivery) (any, error) {
		from := delivery.State
		if err := delivery.Transition(next); err != nil {
			return nil, err
		}
		return transitionedPayload{From: from, To: next}, nil
	})
}

func (s *DeliveryStore) ReviseGoal(ctx context.Context, scope DeliveryScope, id DeliveryID, expectedVersion int64, goal Goal, requirements []Requirement, event EventIdentity) (Delivery, error) {
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	requirements = cloneRequirements(requirements)
	if goal.CreatedAt.IsZero() {
		goal.CreatedAt = event.OccurredAt
	}
	for i := range requirements {
		requirements[i].GoalRevision = goal.Revision
		if requirements[i].Waiver != nil && requirements[i].Waiver.CreatedAt.IsZero() {
			copy := *requirements[i].Waiver
			copy.CreatedAt = event.OccurredAt
			requirements[i].Waiver = &copy
		}
	}
	fingerprint, err := deliveryMutationFingerprint(DeliveryEventGoalRevised, expectedVersion, goalRevisedPayload{Goal: goal, Requirements: requirements})
	if err != nil {
		return Delivery{}, err
	}
	return s.mutate(ctx, scope, id, expectedVersion, DeliveryEventGoalRevised, event, fingerprint, func(ctx context.Context, tx *sql.Tx, delivery *Delivery) (any, error) {
		if goal.Revision != delivery.CurrentGoalRevision+1 || goal.Statement == "" || goal.Actor == "" {
			return nil, ErrInvalidDelivery
		}
		seen := make(map[RequirementID]struct{}, len(requirements))
		for i := range requirements {
			if err := validateRequirement(requirements[i], goal.Revision); err != nil {
				return nil, err
			}
			if _, exists := seen[requirements[i].ID]; exists {
				return nil, ErrInvalidDelivery
			}
			seen[requirements[i].ID] = struct{}{}
		}
		if err := insertGoal(ctx, tx, *delivery, goal, requirements); err != nil {
			return nil, err
		}
		delivery.CurrentGoalRevision = goal.Revision
		delivery.Goals = append(delivery.Goals, goal)
		delivery.Requirements = append(delivery.Requirements, requirements...)
		return goalRevisedPayload{Goal: goal, Requirements: requirements}, nil
	})
}

func (s *DeliveryStore) UpdateRequirement(ctx context.Context, scope DeliveryScope, id DeliveryID, expectedVersion int64, requirementID RequirementID, status RequirementStatus, waiver *RequirementWaiver, event EventIdentity) (Delivery, error) {
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if waiver != nil {
		copy := *waiver
		if copy.CreatedAt.IsZero() {
			copy.CreatedAt = event.OccurredAt
		}
		waiver = &copy
	}
	fingerprint, err := deliveryMutationFingerprint(DeliveryEventRequirementUpdated, expectedVersion, requirementUpdatedPayload{RequirementID: requirementID, Status: status, Waiver: waiver})
	if err != nil {
		return Delivery{}, err
	}
	return s.mutate(ctx, scope, id, expectedVersion, DeliveryEventRequirementUpdated, event, fingerprint, func(ctx context.Context, tx *sql.Tx, delivery *Delivery) (any, error) {
		if requirementID == "" || !stateInRequirement(status, RequirementAccepted, RequirementSatisfied, RequirementWaived) || (status == RequirementWaived) != (waiver != nil) {
			return nil, ErrInvalidDelivery
		}
		if waiver != nil {
			if waiver.Actor == "" || waiver.Reason == "" {
				return nil, ErrInvalidDelivery
			}
		}
		waiverJSON, err := json.Marshal(waiver)
		if err != nil {
			return nil, fmt.Errorf("encode requirement waiver: %w", err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE delivery_requirements SET status = ?, waiver_json = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND goal_revision = ? AND requirement_id = ?`, status, string(waiverJSON), scope.TenantID, scope.RepositoryID, id, delivery.CurrentGoalRevision, requirementID)
		if err != nil {
			return nil, fmt.Errorf("update delivery requirement: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("count updated delivery requirement: %w", err)
		}
		if changed != 1 {
			return nil, ErrDeliveryNotFound
		}
		for i := range delivery.Requirements {
			if delivery.Requirements[i].GoalRevision == delivery.CurrentGoalRevision && delivery.Requirements[i].ID == requirementID {
				delivery.Requirements[i].Status = status
				delivery.Requirements[i].Waiver = waiver
			}
		}
		return requirementUpdatedPayload{RequirementID: requirementID, GoalRevision: delivery.CurrentGoalRevision, Status: status, Waiver: waiver}, nil
	})
}

func (s *DeliveryStore) RequestDecision(ctx context.Context, scope DeliveryScope, id DeliveryID, expectedVersion int64, decision Decision, event EventIdentity) (Delivery, error) {
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if decision.RequestedAt.IsZero() {
		decision.RequestedAt = event.OccurredAt
	}
	fingerprint, err := deliveryMutationFingerprint(DeliveryEventDecisionRequested, expectedVersion, decisionRequestedPayload{Decision: decision})
	if err != nil {
		return Delivery{}, err
	}
	return s.mutate(ctx, scope, id, expectedVersion, DeliveryEventDecisionRequested, event, fingerprint, func(ctx context.Context, tx *sql.Tx, delivery *Delivery) (any, error) {
		if decision.ID == "" || decision.Question == "" || decision.Resolution != nil {
			return nil, ErrInvalidDelivery
		}
		options, _ := json.Marshal(decision.Options)
		blocked, _ := json.Marshal(decision.BlockedDependencies)
		var deadline any
		if decision.Deadline != nil {
			deadline = timestamp(*decision.Deadline)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_decisions (tenant_id, repository_id, delivery_id, decision_id, question, options_json, recommendation, consequences, reversible, deadline, blocked_json, requested_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, scope.TenantID, scope.RepositoryID, id, decision.ID, decision.Question, string(options), decision.Recommendation, decision.Consequences, decision.Reversible, deadline, string(blocked), timestamp(decision.RequestedAt)); err != nil {
			return nil, fmt.Errorf("insert delivery decision: %w", err)
		}
		delivery.Decisions = append(delivery.Decisions, decision)
		return decisionRequestedPayload{Decision: decision}, nil
	})
}

func (s *DeliveryStore) ResolveDecision(ctx context.Context, scope DeliveryScope, id DeliveryID, expectedVersion int64, decisionID DecisionID, resolution DecisionResolution, event EventIdentity) (Delivery, error) {
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if resolution.ResolvedAt.IsZero() {
		resolution.ResolvedAt = event.OccurredAt
	}
	fingerprint, err := deliveryMutationFingerprint(DeliveryEventDecisionResolved, expectedVersion, decisionResolvedPayload{DecisionID: decisionID, Resolution: resolution})
	if err != nil {
		return Delivery{}, err
	}
	return s.mutate(ctx, scope, id, expectedVersion, DeliveryEventDecisionResolved, event, fingerprint, func(ctx context.Context, tx *sql.Tx, delivery *Delivery) (any, error) {
		if decisionID == "" || resolution.Actor == "" || resolution.Choice == "" {
			return nil, ErrInvalidDelivery
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO delivery_decision_resolutions (tenant_id, repository_id, delivery_id, decision_id, actor, choice, rationale, resolved_at) SELECT tenant_id, repository_id, delivery_id, decision_id, ?, ?, ?, ? FROM delivery_decisions WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND decision_id = ?`, resolution.Actor, resolution.Choice, resolution.Rationale, timestamp(resolution.ResolvedAt), scope.TenantID, scope.RepositoryID, id, decisionID)
		if err != nil {
			return nil, fmt.Errorf("resolve delivery decision: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("count resolved delivery decision: %w", err)
		}
		if changed != 1 {
			return nil, ErrDeliveryNotFound
		}
		for i := range delivery.Decisions {
			if delivery.Decisions[i].ID == decisionID {
				copy := resolution
				delivery.Decisions[i].Resolution = &copy
			}
		}
		return decisionResolvedPayload{DecisionID: decisionID, Resolution: resolution}, nil
	})
}

type deliveryMutation func(context.Context, *sql.Tx, *Delivery) (any, error)

func (s *DeliveryStore) mutate(ctx context.Context, scope DeliveryScope, id DeliveryID, expectedVersion int64, eventType DeliveryEventType, event EventIdentity, fingerprint string, mutation deliveryMutation) (Delivery, error) {
	if err := validateDeliveryRef(scope, id); err != nil || expectedVersion < 1 || event.ID == "" || event.IdempotencyKey == "" {
		return Delivery{}, ErrInvalidDelivery
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, fmt.Errorf("begin delivery mutation: %w", err)
	}
	defer tx.Rollback()
	if existing, found, err := loadExistingMutation(ctx, tx, scope, id, eventType, event, fingerprint); err != nil || found {
		return existing, err
	}
	delivery, err := loadDelivery(ctx, tx, scope, id)
	if err != nil {
		return Delivery{}, err
	}
	if delivery.Version != expectedVersion {
		return Delivery{}, ErrStaleDeliveryVersion
	}
	payloadValue, err := mutation(ctx, tx, &delivery)
	if err != nil {
		if !errors.Is(err, ErrInvalidDelivery) && !errors.Is(err, ErrInvalidDeliveryTransition) && !errors.Is(err, ErrDeliveryNotFound) {
			return s.resolveMutationFailure(ctx, tx, scope, id, expectedVersion, eventType, event, fingerprint, err)
		}
		return Delivery{}, err
	}
	delivery.Version++
	delivery.UpdatedAt = event.OccurredAt
	payload, err := json.Marshal(withMutationVersion(payloadValue, delivery.Version))
	if err != nil {
		return Delivery{}, fmt.Errorf("encode delivery mutation event: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE delivery_deliveries SET state = ?, version = ?, current_goal_revision = ?, updated_at = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND version = ?`, delivery.State, delivery.Version, delivery.CurrentGoalRevision, timestamp(delivery.UpdatedAt), scope.TenantID, scope.RepositoryID, id, expectedVersion)
	if err != nil {
		return s.resolveMutationFailure(ctx, tx, scope, id, expectedVersion, eventType, event, fingerprint, fmt.Errorf("update delivery: %w", err))
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Delivery{}, fmt.Errorf("count delivery update: %w", err)
	}
	if changed != 1 {
		return Delivery{}, ErrStaleDeliveryVersion
	}
	if _, err := appendDeliveryEvent(ctx, tx, scope, id, eventType, payload, event, fingerprint); err != nil {
		return s.resolveMutationFailure(ctx, tx, scope, id, expectedVersion, eventType, event, fingerprint, err)
	}
	if err := tx.Commit(); err != nil {
		return s.resolveMutationFailure(ctx, tx, scope, id, expectedVersion, eventType, event, fingerprint, fmt.Errorf("commit delivery mutation: %w", err))
	}
	return delivery, nil
}

func (s *DeliveryStore) resolveMutationFailure(ctx context.Context, tx *sql.Tx, scope DeliveryScope, id DeliveryID, expectedVersion int64, eventType DeliveryEventType, event EventIdentity, fingerprint string, original error) (Delivery, error) {
	_ = tx.Rollback()
	for attempt := 0; attempt < 100; attempt++ {
		if existing, found, err := loadExistingMutation(ctx, s.db, scope, id, eventType, event, fingerprint); err != nil || found {
			return existing, err
		}
		current, err := loadDelivery(ctx, s.db, scope, id)
		if err == nil && current.Version != expectedVersion {
			// A competing transaction may commit between the first identity
			// lookup and this version read. Resolve identity before returning a
			// generic stale-version error for the very same event key.
			if existing, found, err := loadExistingMutation(ctx, s.db, scope, id, eventType, event, fingerprint); err != nil || found {
				return existing, err
			}
			return Delivery{}, ErrStaleDeliveryVersion
		}
		select {
		case <-ctx.Done():
			return Delivery{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return Delivery{}, original
}

// AppendEvent persists an attributed delivery event for later F05/F06 records.
// Aggregate-changing event types must use their corresponding CAS methods.
func (s *DeliveryStore) AppendEvent(ctx context.Context, scope DeliveryScope, id DeliveryID, eventType DeliveryEventType, payload json.RawMessage, identity EventIdentity) (DeliveryEvent, error) {
	if err := validateDeliveryRef(scope, id); err != nil || eventType == "" || identity.ID == "" || identity.IdempotencyKey == "" {
		return DeliveryEvent{}, ErrInvalidDelivery
	}
	if stateInEvent(eventType, DeliveryEventAdmitted, DeliveryEventTransitioned, DeliveryEventGoalRevised, DeliveryEventRequirementUpdated, DeliveryEventDecisionRequested, DeliveryEventDecisionResolved) {
		return DeliveryEvent{}, ErrInvalidDelivery
	}
	if identity.OccurredAt.IsZero() {
		identity.OccurredAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryEvent{}, fmt.Errorf("begin append delivery event: %w", err)
	}
	defer tx.Rollback()
	if _, err := loadDelivery(ctx, tx, scope, id); err != nil {
		return DeliveryEvent{}, err
	}
	event, err := appendDeliveryEvent(ctx, tx, scope, id, eventType, payload, identity, "")
	if err != nil {
		return DeliveryEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryEvent{}, fmt.Errorf("commit delivery event: %w", err)
	}
	return event, nil
}

func (s *DeliveryStore) Events(ctx context.Context, scope DeliveryScope, id DeliveryID) ([]DeliveryEvent, error) {
	if err := validateDeliveryRef(scope, id); err != nil {
		return nil, err
	}
	if _, err := s.Load(ctx, scope, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, sequence, event_type, idempotency_key, occurred_at, payload FROM delivery_events WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? ORDER BY sequence`, scope.TenantID, scope.RepositoryID, id)
	if err != nil {
		return nil, fmt.Errorf("list delivery events: %w", err)
	}
	defer rows.Close()
	var events []DeliveryEvent
	for rows.Next() {
		var event DeliveryEvent
		var occurredAt, payload string
		if err := rows.Scan(&event.ID, &event.Sequence, &event.Type, &event.IdempotencyKey, &occurredAt, &payload); err != nil {
			return nil, fmt.Errorf("scan delivery event: %w", err)
		}
		if event.OccurredAt, err = parseTimestamp(occurredAt); err != nil {
			return nil, err
		}
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list delivery events: %w", err)
	}
	return events, nil
}

// Replay deterministically reconstructs aggregate state from ordered events.
func ReplayDelivery(events []DeliveryEvent) (Delivery, error) {
	var delivery Delivery
	for i, event := range events {
		if event.Sequence != uint64(i+1) {
			return Delivery{}, fmt.Errorf("delivery event sequence %d: %w", event.Sequence, ErrInvalidDelivery)
		}
		switch event.Type {
		case DeliveryEventAdmitted:
			if i != 0 {
				return Delivery{}, ErrInvalidDelivery
			}
			var payload admittedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return Delivery{}, fmt.Errorf("decode admitted delivery event: %w", err)
			}
			delivery = payload.Delivery
		case DeliveryEventTransitioned:
			var payload versionedTransitionedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil || delivery.State != payload.From || !validDeliveryTransition(payload.From, payload.To) || payload.Version != delivery.Version+1 {
				return Delivery{}, ErrInvalidDelivery
			}
			delivery.State, delivery.Version = payload.To, payload.Version
			delivery.UpdatedAt = event.OccurredAt
		case DeliveryEventGoalRevised:
			var payload versionedGoalRevisedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Version != delivery.Version+1 || payload.Goal.Revision != delivery.CurrentGoalRevision+1 {
				return Delivery{}, ErrInvalidDelivery
			}
			delivery.Goals = append(delivery.Goals, payload.Goal)
			delivery.Requirements = append(delivery.Requirements, payload.Requirements...)
			delivery.CurrentGoalRevision, delivery.Version, delivery.UpdatedAt = payload.Goal.Revision, payload.Version, event.OccurredAt
		case DeliveryEventRequirementUpdated:
			var payload versionedRequirementUpdatedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Version != delivery.Version+1 {
				return Delivery{}, ErrInvalidDelivery
			}
			found := false
			for j := range delivery.Requirements {
				if delivery.Requirements[j].ID == payload.RequirementID && delivery.Requirements[j].GoalRevision == payload.GoalRevision {
					delivery.Requirements[j].Status, delivery.Requirements[j].Waiver, found = payload.Status, payload.Waiver, true
				}
			}
			if !found {
				return Delivery{}, ErrInvalidDelivery
			}
			delivery.Version, delivery.UpdatedAt = payload.Version, event.OccurredAt
		case DeliveryEventDecisionRequested:
			var payload versionedDecisionRequestedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Version != delivery.Version+1 {
				return Delivery{}, ErrInvalidDelivery
			}
			delivery.Decisions = append(delivery.Decisions, payload.Decision)
			delivery.Version, delivery.UpdatedAt = payload.Version, event.OccurredAt
		case DeliveryEventDecisionResolved:
			var payload versionedDecisionResolvedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Version != delivery.Version+1 {
				return Delivery{}, ErrInvalidDelivery
			}
			found := false
			for j := range delivery.Decisions {
				if delivery.Decisions[j].ID == payload.DecisionID && delivery.Decisions[j].Resolution == nil {
					copy := payload.Resolution
					delivery.Decisions[j].Resolution, found = &copy, true
				}
			}
			if !found {
				return Delivery{}, ErrInvalidDelivery
			}
			delivery.Version, delivery.UpdatedAt = payload.Version, event.OccurredAt
		default:
			// Extension events do not alter this projection.
		}
	}
	if delivery.ID == "" {
		return Delivery{}, ErrInvalidDelivery
	}
	return delivery, nil
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadDelivery(ctx context.Context, db queryer, scope DeliveryScope, id DeliveryID) (Delivery, error) {
	var delivery Delivery
	delivery.DeliveryScope, delivery.ID = scope, id
	var createdAt, updatedAt string
	err := db.QueryRowContext(ctx, `SELECT admission_key, state, version, current_goal_revision, policy_reference, max_cost_microdollars, created_at, updated_at FROM delivery_deliveries WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?`, scope.TenantID, scope.RepositoryID, id).Scan(&delivery.AdmissionKey, &delivery.State, &delivery.Version, &delivery.CurrentGoalRevision, &delivery.PolicyReference, &delivery.MaxCostMicrodollars, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, ErrDeliveryNotFound
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("load delivery: %w", err)
	}
	if delivery.CreatedAt, err = parseTimestamp(createdAt); err != nil {
		return Delivery{}, err
	}
	if delivery.UpdatedAt, err = parseTimestamp(updatedAt); err != nil {
		return Delivery{}, err
	}
	if err := loadGoalsAndRequirements(ctx, db, &delivery); err != nil {
		return Delivery{}, err
	}
	if err := loadDecisions(ctx, db, &delivery); err != nil {
		return Delivery{}, err
	}
	return delivery, nil
}

func loadGoalsAndRequirements(ctx context.Context, db queryer, delivery *Delivery) error {
	rows, err := db.QueryContext(ctx, `SELECT revision, statement, actor, created_at FROM delivery_goals WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? ORDER BY revision`, delivery.TenantID, delivery.RepositoryID, delivery.ID)
	if err != nil {
		return fmt.Errorf("load delivery goals: %w", err)
	}
	for rows.Next() {
		var goal Goal
		var createdAt string
		if err := rows.Scan(&goal.Revision, &goal.Statement, &goal.Actor, &createdAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan delivery goal: %w", err)
		}
		if goal.CreatedAt, err = parseTimestamp(createdAt); err != nil {
			rows.Close()
			return err
		}
		delivery.Goals = append(delivery.Goals, goal)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("load delivery goals: %w", err)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT requirement_id, goal_revision, statement, status, waiver_json FROM delivery_requirements WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? ORDER BY goal_revision, requirement_id`, delivery.TenantID, delivery.RepositoryID, delivery.ID)
	if err != nil {
		return fmt.Errorf("load delivery requirements: %w", err)
	}
	for rows.Next() {
		var requirement Requirement
		var waiverJSON string
		if err := rows.Scan(&requirement.ID, &requirement.GoalRevision, &requirement.Statement, &requirement.Status, &waiverJSON); err != nil {
			rows.Close()
			return fmt.Errorf("scan delivery requirement: %w", err)
		}
		if waiverJSON != "null" && waiverJSON != "" {
			if err := json.Unmarshal([]byte(waiverJSON), &requirement.Waiver); err != nil {
				rows.Close()
				return fmt.Errorf("decode delivery requirement waiver: %w", err)
			}
		}
		delivery.Requirements = append(delivery.Requirements, requirement)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("load delivery requirements: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close delivery requirements: %w", err)
	}
	for i := range delivery.Requirements {
		checks, err := loadAcceptanceChecks(ctx, db, *delivery, delivery.Requirements[i].GoalRevision, delivery.Requirements[i].ID)
		if err != nil {
			return err
		}
		delivery.Requirements[i].Checks = checks
	}
	return nil
}

func loadAcceptanceChecks(ctx context.Context, db queryer, delivery Delivery, revision GoalRevision, requirementID RequirementID) ([]AcceptanceCheck, error) {
	rows, err := db.QueryContext(ctx, `SELECT check_id, statement FROM delivery_acceptance_checks WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND goal_revision = ? AND requirement_id = ? ORDER BY position`, delivery.TenantID, delivery.RepositoryID, delivery.ID, revision, requirementID)
	if err != nil {
		return nil, fmt.Errorf("load delivery acceptance checks: %w", err)
	}
	defer rows.Close()
	var checks []AcceptanceCheck
	for rows.Next() {
		var check AcceptanceCheck
		if err := rows.Scan(&check.ID, &check.Statement); err != nil {
			return nil, fmt.Errorf("scan delivery acceptance check: %w", err)
		}
		checks = append(checks, check)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load delivery acceptance checks: %w", err)
	}
	return checks, nil
}

func loadDecisions(ctx context.Context, db queryer, delivery *Delivery) error {
	rows, err := db.QueryContext(ctx, `SELECT d.decision_id, d.question, d.options_json, d.recommendation, d.consequences, d.reversible, d.deadline, d.blocked_json, d.requested_at, r.actor, r.choice, r.rationale, r.resolved_at FROM delivery_decisions d LEFT JOIN delivery_decision_resolutions r ON r.tenant_id = d.tenant_id AND r.repository_id = d.repository_id AND r.delivery_id = d.delivery_id AND r.decision_id = d.decision_id WHERE d.tenant_id = ? AND d.repository_id = ? AND d.delivery_id = ? ORDER BY d.requested_at, d.decision_id`, delivery.TenantID, delivery.RepositoryID, delivery.ID)
	if err != nil {
		return fmt.Errorf("load delivery decisions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var decision Decision
		var optionsJSON, blockedJSON, requestedAt string
		var deadline, actor, choice, rationale, resolvedAt sql.NullString
		if err := rows.Scan(&decision.ID, &decision.Question, &optionsJSON, &decision.Recommendation, &decision.Consequences, &decision.Reversible, &deadline, &blockedJSON, &requestedAt, &actor, &choice, &rationale, &resolvedAt); err != nil {
			return fmt.Errorf("scan delivery decision: %w", err)
		}
		if err := json.Unmarshal([]byte(optionsJSON), &decision.Options); err != nil {
			return fmt.Errorf("decode delivery decision options: %w", err)
		}
		if err := json.Unmarshal([]byte(blockedJSON), &decision.BlockedDependencies); err != nil {
			return fmt.Errorf("decode delivery decision dependencies: %w", err)
		}
		if decision.RequestedAt, err = parseTimestamp(requestedAt); err != nil {
			return err
		}
		if deadline.Valid {
			parsed, err := parseTimestamp(deadline.String)
			if err != nil {
				return err
			}
			decision.Deadline = &parsed
		}
		if actor.Valid {
			parsed, err := parseTimestamp(resolvedAt.String)
			if err != nil {
				return err
			}
			decision.Resolution = &DecisionResolution{Actor: actor.String, Choice: choice.String, Rationale: rationale.String, ResolvedAt: parsed}
		}
		delivery.Decisions = append(delivery.Decisions, decision)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load delivery decisions: %w", err)
	}
	return nil
}

func loadExistingMutation(ctx context.Context, db queryer, scope DeliveryScope, id DeliveryID, eventType DeliveryEventType, identity EventIdentity, fingerprint string) (Delivery, bool, error) {
	var eventID DeliveryEventID
	var key DeliveryIdempotencyKey
	var existingType DeliveryEventType
	var occurredAt, existingFingerprint string
	var sequence uint64
	err := db.QueryRowContext(ctx, `SELECT event_id, idempotency_key, event_type, occurred_at, request_fingerprint, sequence FROM delivery_events WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND (event_id = ? OR idempotency_key = ?)`, scope.TenantID, scope.RepositoryID, id, identity.ID, identity.IdempotencyKey).Scan(&eventID, &key, &existingType, &occurredAt, &existingFingerprint, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, false, nil
	}
	if err != nil {
		return Delivery{}, false, fmt.Errorf("check duplicate delivery mutation: %w", err)
	}
	if eventID != identity.ID || key != identity.IdempotencyKey || existingType != eventType || existingFingerprint != fingerprint {
		return Delivery{}, false, ErrEventConflict
	}
	events, err := loadDeliveryEventsThrough(ctx, db, scope, id, sequence)
	if err != nil {
		return Delivery{}, false, err
	}
	delivery, err := ReplayDelivery(events)
	return delivery, err == nil, err
}

func loadDeliveryEventsThrough(ctx context.Context, db queryer, scope DeliveryScope, id DeliveryID, sequence uint64) ([]DeliveryEvent, error) {
	rows, err := db.QueryContext(ctx, `SELECT event_id, sequence, event_type, idempotency_key, occurred_at, payload FROM delivery_events WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND sequence <= ? ORDER BY sequence`, scope.TenantID, scope.RepositoryID, id, sequence)
	if err != nil {
		return nil, fmt.Errorf("load delivery mutation events: %w", err)
	}
	defer rows.Close()
	var events []DeliveryEvent
	for rows.Next() {
		var event DeliveryEvent
		var occurredAt, payload string
		if err := rows.Scan(&event.ID, &event.Sequence, &event.Type, &event.IdempotencyKey, &occurredAt, &payload); err != nil {
			return nil, fmt.Errorf("scan delivery mutation event: %w", err)
		}
		if event.OccurredAt, err = parseTimestamp(occurredAt); err != nil {
			return nil, err
		}
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load delivery mutation events: %w", err)
	}
	return events, nil
}

func insertGoal(ctx context.Context, tx *sql.Tx, delivery Delivery, goal Goal, requirements []Requirement) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_goals (tenant_id, repository_id, delivery_id, revision, statement, actor, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, delivery.TenantID, delivery.RepositoryID, delivery.ID, goal.Revision, goal.Statement, goal.Actor, timestamp(goal.CreatedAt)); err != nil {
		return fmt.Errorf("insert delivery goal: %w", err)
	}
	for _, requirement := range requirements {
		waiver, err := json.Marshal(requirement.Waiver)
		if err != nil {
			return fmt.Errorf("encode delivery requirement waiver: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_requirements (tenant_id, repository_id, delivery_id, goal_revision, requirement_id, statement, status, waiver_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, delivery.TenantID, delivery.RepositoryID, delivery.ID, requirement.GoalRevision, requirement.ID, requirement.Statement, requirement.Status, string(waiver)); err != nil {
			return fmt.Errorf("insert delivery requirement: %w", err)
		}
		for position, check := range requirement.Checks {
			if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_acceptance_checks (tenant_id, repository_id, delivery_id, goal_revision, requirement_id, check_id, position, statement) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, delivery.TenantID, delivery.RepositoryID, delivery.ID, requirement.GoalRevision, requirement.ID, check.ID, position, check.Statement); err != nil {
				return fmt.Errorf("insert delivery acceptance check: %w", err)
			}
		}
	}
	return nil
}

func appendDeliveryEvent(ctx context.Context, tx *sql.Tx, scope DeliveryScope, id DeliveryID, eventType DeliveryEventType, payload json.RawMessage, identity EventIdentity, requestFingerprint string) (DeliveryEvent, error) {
	var existing DeliveryEvent
	var occurredAt, existingPayload string
	var existingKey DeliveryIdempotencyKey
	err := tx.QueryRowContext(ctx, `SELECT event_id, sequence, event_type, idempotency_key, occurred_at, payload FROM delivery_events WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND (idempotency_key = ? OR event_id = ?)`, scope.TenantID, scope.RepositoryID, id, identity.IdempotencyKey, identity.ID).Scan(&existing.ID, &existing.Sequence, &existing.Type, &existingKey, &occurredAt, &existingPayload)
	if err == nil {
		if existing.ID != identity.ID || existingKey != identity.IdempotencyKey || existing.Type != eventType || existingPayload != string(payload) || occurredAt != timestamp(identity.OccurredAt) {
			return DeliveryEvent{}, ErrEventConflict
		}
		existing.IdempotencyKey = existingKey
		existing.OccurredAt, _ = parseTimestamp(occurredAt)
		existing.Payload = json.RawMessage(existingPayload)
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DeliveryEvent{}, fmt.Errorf("check duplicate delivery event: %w", err)
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM delivery_events WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ?`, scope.TenantID, scope.RepositoryID, id).Scan(&sequence); err != nil {
		return DeliveryEvent{}, fmt.Errorf("allocate delivery event sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_events (tenant_id, repository_id, delivery_id, sequence, event_id, idempotency_key, event_type, occurred_at, payload, request_fingerprint) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, scope.TenantID, scope.RepositoryID, id, sequence, identity.ID, identity.IdempotencyKey, eventType, timestamp(identity.OccurredAt), string(payload), requestFingerprint); err != nil {
		return DeliveryEvent{}, fmt.Errorf("insert delivery event: %w", err)
	}
	return DeliveryEvent{ID: identity.ID, Sequence: sequence, Type: eventType, IdempotencyKey: identity.IdempotencyKey, OccurredAt: identity.OccurredAt, Payload: payload}, nil
}

func normalizeAdmission(admission Admission) (Admission, string, error) {
	if err := validateDeliveryRef(admission.Scope, admission.DeliveryID); err != nil || admission.AdmissionKey == "" || admission.Goal.Statement == "" || admission.Goal.Actor == "" || admission.Event.ID == "" || admission.Event.IdempotencyKey == "" || admission.MaxCostMicrodollars < 0 {
		return Admission{}, "", ErrInvalidDelivery
	}
	admission.Goal.Revision = 1
	admission.Requirements = cloneRequirements(admission.Requirements)
	if admission.Event.OccurredAt.IsZero() {
		admission.Event.OccurredAt = time.Now().UTC()
	}
	if admission.Goal.CreatedAt.IsZero() {
		admission.Goal.CreatedAt = admission.Event.OccurredAt
	}
	seen := make(map[RequirementID]struct{}, len(admission.Requirements))
	for i := range admission.Requirements {
		admission.Requirements[i].GoalRevision = 1
		if admission.Requirements[i].Waiver != nil && admission.Requirements[i].Waiver.CreatedAt.IsZero() {
			copy := *admission.Requirements[i].Waiver
			copy.CreatedAt = admission.Event.OccurredAt
			admission.Requirements[i].Waiver = &copy
		}
		if err := validateRequirement(admission.Requirements[i], 1); err != nil {
			return Admission{}, "", err
		}
		if _, exists := seen[admission.Requirements[i].ID]; exists {
			return Admission{}, "", ErrInvalidDelivery
		}
		seen[admission.Requirements[i].ID] = struct{}{}
	}
	fingerprintInput := struct {
		DeliveryID          DeliveryID
		Goal                Goal
		Requirements        []Requirement
		PolicyReference     string
		MaxCostMicrodollars int64
		EventID             DeliveryEventID
		EventIdempotencyKey DeliveryIdempotencyKey
	}{admission.DeliveryID, admission.Goal, make([]Requirement, len(admission.Requirements)), admission.PolicyReference, admission.MaxCostMicrodollars, admission.Event.ID, admission.Event.IdempotencyKey}
	copy(fingerprintInput.Requirements, admission.Requirements)
	// Timestamps are metadata, not semantic admission content, so retries may be
	// issued after reconnect without manufacturing a conflict.
	fingerprintInput.Goal.CreatedAt = time.Time{}
	for i := range fingerprintInput.Requirements {
		if fingerprintInput.Requirements[i].Waiver != nil {
			copy := *fingerprintInput.Requirements[i].Waiver
			copy.CreatedAt = time.Time{}
			fingerprintInput.Requirements[i].Waiver = &copy
		}
	}
	encoded, err := json.Marshal(fingerprintInput)
	if err != nil {
		return Admission{}, "", fmt.Errorf("encode delivery admission fingerprint: %w", err)
	}
	hash := sha256.Sum256(encoded)
	return admission, hex.EncodeToString(hash[:]), nil
}

func cloneRequirements(requirements []Requirement) []Requirement {
	cloned := make([]Requirement, len(requirements))
	for i, requirement := range requirements {
		cloned[i] = requirement
		cloned[i].Checks = append([]AcceptanceCheck(nil), requirement.Checks...)
		if requirement.Waiver != nil {
			waiver := *requirement.Waiver
			cloned[i].Waiver = &waiver
		}
	}
	return cloned
}

func validateDeliveryRef(scope DeliveryScope, id DeliveryID) error {
	if err := scope.Validate(); err != nil || id == "" {
		return ErrInvalidDelivery
	}
	return nil
}

func timestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse delivery timestamp: %w", err)
	}
	return parsed, nil
}

func deliverySchemaChecksum(schema string) string {
	hash := sha256.Sum256([]byte(schema))
	return hex.EncodeToString(hash[:])
}

func deliveryMutationFingerprint(eventType DeliveryEventType, expectedVersion int64, payload any) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode delivery mutation fingerprint: %w", err)
	}
	var request map[string]any
	if err := json.Unmarshal(encoded, &request); err != nil {
		return "", fmt.Errorf("normalize delivery mutation fingerprint: %w", err)
	}
	delete(request, "version")
	if eventType == DeliveryEventTransitioned {
		delete(request, "from")
	}
	if eventType == DeliveryEventRequirementUpdated {
		delete(request, "goal_revision")
	}
	stripDeliveryMutationMetadata(eventType, request)
	canonical, err := json.Marshal(struct {
		EventType       DeliveryEventType `json:"event_type"`
		ExpectedVersion int64             `json:"expected_version"`
		Request         map[string]any    `json:"request"`
	}{eventType, expectedVersion, request})
	if err != nil {
		return "", fmt.Errorf("encode delivery mutation fingerprint: %w", err)
	}
	hash := sha256.Sum256(canonical)
	return hex.EncodeToString(hash[:]), nil
}

func stripDeliveryMutationMetadata(eventType DeliveryEventType, request map[string]any) {
	deleteNestedField := func(value any, field string) {
		if object, ok := value.(map[string]any); ok {
			delete(object, field)
		}
	}
	switch eventType {
	case DeliveryEventGoalRevised:
		deleteNestedField(request["goal"], "created_at")
		if requirements, ok := request["requirements"].([]any); ok {
			for _, requirement := range requirements {
				if object, ok := requirement.(map[string]any); ok {
					deleteNestedField(object["waiver"], "created_at")
				}
			}
		}
	case DeliveryEventRequirementUpdated:
		deleteNestedField(request["waiver"], "created_at")
	case DeliveryEventDecisionRequested:
		deleteNestedField(request["decision"], "requested_at")
	case DeliveryEventDecisionResolved:
		deleteNestedField(request["resolution"], "resolved_at")
	}
}

func backfillDeliveryMutationFingerprints(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT tenant_id, repository_id, delivery_id, sequence, event_type, payload FROM delivery_events WHERE event_type IN (?, ?, ?, ?, ?)`, DeliveryEventTransitioned, DeliveryEventGoalRevised, DeliveryEventRequirementUpdated, DeliveryEventDecisionRequested, DeliveryEventDecisionResolved)
	if err != nil {
		return fmt.Errorf("read delivery mutation fingerprints: %w", err)
	}
	type update struct {
		tenant, repository, delivery string
		sequence                     uint64
		fingerprint                  string
	}
	var updates []update
	for rows.Next() {
		var item update
		var eventType DeliveryEventType
		var payload string
		if err := rows.Scan(&item.tenant, &item.repository, &item.delivery, &item.sequence, &eventType, &payload); err != nil {
			rows.Close()
			return fmt.Errorf("scan delivery mutation fingerprint: %w", err)
		}
		var versioned struct {
			Version int64 `json:"version"`
		}
		if err := json.Unmarshal([]byte(payload), &versioned); err != nil || versioned.Version < 2 {
			rows.Close()
			return ErrIncompatibleDeliverySchema
		}
		item.fingerprint, err = deliveryMutationFingerprint(eventType, versioned.Version-1, json.RawMessage(payload))
		if err != nil {
			rows.Close()
			return err
		}
		updates = append(updates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read delivery mutation fingerprints: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close delivery mutation fingerprints: %w", err)
	}
	for _, item := range updates {
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_events SET request_fingerprint = ? WHERE tenant_id = ? AND repository_id = ? AND delivery_id = ? AND sequence = ?`, item.fingerprint, item.tenant, item.repository, item.delivery, item.sequence); err != nil {
			return fmt.Errorf("backfill delivery mutation fingerprint: %w", err)
		}
	}
	return nil
}

func stateInEvent(event DeliveryEventType, allowed ...DeliveryEventType) bool {
	for _, candidate := range allowed {
		if event == candidate {
			return true
		}
	}
	return false
}

type admittedPayload struct {
	Delivery Delivery `json:"delivery"`
}

type transitionedPayload struct {
	From DeliveryState `json:"from"`
	To   DeliveryState `json:"to"`
}

type goalRevisedPayload struct {
	Goal         Goal          `json:"goal"`
	Requirements []Requirement `json:"requirements"`
}

type requirementUpdatedPayload struct {
	RequirementID RequirementID      `json:"requirement_id"`
	GoalRevision  GoalRevision       `json:"goal_revision"`
	Status        RequirementStatus  `json:"status"`
	Waiver        *RequirementWaiver `json:"waiver,omitempty"`
}

type decisionRequestedPayload struct {
	Decision Decision `json:"decision"`
}

type decisionResolvedPayload struct {
	DecisionID DecisionID         `json:"decision_id"`
	Resolution DecisionResolution `json:"resolution"`
}

type versionedTransitionedPayload struct {
	transitionedPayload
	Version int64 `json:"version"`
}

type versionedGoalRevisedPayload struct {
	goalRevisedPayload
	Version int64 `json:"version"`
}

type versionedRequirementUpdatedPayload struct {
	requirementUpdatedPayload
	Version int64 `json:"version"`
}

type versionedDecisionRequestedPayload struct {
	decisionRequestedPayload
	Version int64 `json:"version"`
}

type versionedDecisionResolvedPayload struct {
	decisionResolvedPayload
	Version int64 `json:"version"`
}

func withMutationVersion(payload any, version int64) any {
	switch value := payload.(type) {
	case transitionedPayload:
		return versionedTransitionedPayload{transitionedPayload: value, Version: version}
	case goalRevisedPayload:
		return versionedGoalRevisedPayload{goalRevisedPayload: value, Version: version}
	case requirementUpdatedPayload:
		return versionedRequirementUpdatedPayload{requirementUpdatedPayload: value, Version: version}
	case decisionRequestedPayload:
		return versionedDecisionRequestedPayload{decisionRequestedPayload: value, Version: version}
	case decisionResolvedPayload:
		return versionedDecisionResolvedPayload{decisionResolvedPayload: value, Version: version}
	default:
		panic("unsupported delivery mutation payload")
	}
}

const deliverySchemaV1 = `
CREATE TABLE delivery_deliveries (
 tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, delivery_id TEXT NOT NULL,
 admission_key TEXT NOT NULL, admission_fingerprint TEXT NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('admitted','queued','running','checkpointing','waiting_retry','waiting_decision','waiting_credentials','waiting_quota','paused','reconciling','replanning','cancel_requested','succeeded','failed','cancelled')),
 version INTEGER NOT NULL CHECK (version > 0), current_goal_revision INTEGER NOT NULL CHECK (current_goal_revision > 0),
 policy_reference TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 PRIMARY KEY (tenant_id, repository_id, delivery_id), UNIQUE (tenant_id, repository_id, admission_key)
);
CREATE TABLE delivery_goals (
 tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, delivery_id TEXT NOT NULL, revision INTEGER NOT NULL,
 statement TEXT NOT NULL, actor TEXT NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY (tenant_id, repository_id, delivery_id, revision),
 FOREIGN KEY (tenant_id, repository_id, delivery_id) REFERENCES delivery_deliveries ON DELETE CASCADE
);
CREATE TABLE delivery_requirements (
 tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, delivery_id TEXT NOT NULL, goal_revision INTEGER NOT NULL,
 requirement_id TEXT NOT NULL, statement TEXT NOT NULL,
 status TEXT NOT NULL CHECK (status IN ('accepted','satisfied','waived')), waiver_json TEXT NOT NULL DEFAULT 'null',
 PRIMARY KEY (tenant_id, repository_id, delivery_id, goal_revision, requirement_id),
 FOREIGN KEY (tenant_id, repository_id, delivery_id, goal_revision) REFERENCES delivery_goals ON DELETE CASCADE
);
CREATE TABLE delivery_acceptance_checks (
 tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, delivery_id TEXT NOT NULL, goal_revision INTEGER NOT NULL,
 requirement_id TEXT NOT NULL, check_id TEXT NOT NULL, position INTEGER NOT NULL, statement TEXT NOT NULL,
 PRIMARY KEY (tenant_id, repository_id, delivery_id, goal_revision, requirement_id, check_id),
 UNIQUE (tenant_id, repository_id, delivery_id, goal_revision, requirement_id, position),
 FOREIGN KEY (tenant_id, repository_id, delivery_id, goal_revision, requirement_id) REFERENCES delivery_requirements ON DELETE CASCADE
);
CREATE TABLE delivery_decisions (
 tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, delivery_id TEXT NOT NULL, decision_id TEXT NOT NULL,
 question TEXT NOT NULL, options_json TEXT NOT NULL, recommendation TEXT NOT NULL, consequences TEXT NOT NULL,
 reversible INTEGER NOT NULL, deadline TEXT, blocked_json TEXT NOT NULL, requested_at TEXT NOT NULL,
 PRIMARY KEY (tenant_id, repository_id, delivery_id, decision_id),
 FOREIGN KEY (tenant_id, repository_id, delivery_id) REFERENCES delivery_deliveries ON DELETE CASCADE
);
CREATE TABLE delivery_decision_resolutions (
 tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, delivery_id TEXT NOT NULL, decision_id TEXT NOT NULL,
 actor TEXT NOT NULL, choice TEXT NOT NULL, rationale TEXT NOT NULL, resolved_at TEXT NOT NULL,
 PRIMARY KEY (tenant_id, repository_id, delivery_id, decision_id),
 FOREIGN KEY (tenant_id, repository_id, delivery_id, decision_id) REFERENCES delivery_decisions ON DELETE CASCADE
);
CREATE TABLE delivery_events (
 tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, delivery_id TEXT NOT NULL, sequence INTEGER NOT NULL,
 event_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, event_type TEXT NOT NULL, occurred_at TEXT NOT NULL, payload TEXT NOT NULL,
 PRIMARY KEY (tenant_id, repository_id, delivery_id, sequence),
 UNIQUE (tenant_id, repository_id, delivery_id, event_id),
 UNIQUE (tenant_id, repository_id, delivery_id, idempotency_key),
 FOREIGN KEY (tenant_id, repository_id, delivery_id) REFERENCES delivery_deliveries ON DELETE CASCADE
);
CREATE INDEX idx_delivery_state ON delivery_deliveries (tenant_id, repository_id, state, created_at, delivery_id);
CREATE INDEX idx_delivery_events_order ON delivery_events (tenant_id, repository_id, delivery_id, sequence);`

const deliverySchemaV2 = `ALTER TABLE delivery_events ADD COLUMN request_fingerprint TEXT NOT NULL DEFAULT '';`

const deliverySchemaV3 = `
CREATE TABLE delivery_queue (
 tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, delivery_id TEXT NOT NULL,
 status TEXT NOT NULL CHECK (status IN ('ready','leased','waiting','done','cancelled')),
 available_at TEXT NOT NULL, waiting_signal TEXT NOT NULL DEFAULT '',
 lease_owner_id TEXT, lease_epoch INTEGER NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
 lease_acquired_at TEXT, lease_expires_at TEXT, lease_heartbeat_at TEXT,
 result_json TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 PRIMARY KEY (tenant_id, repository_id, delivery_id),
 FOREIGN KEY (tenant_id, repository_id, delivery_id) REFERENCES delivery_deliveries ON DELETE CASCADE
);
CREATE TABLE delivery_worker_attempts (
 tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, delivery_id TEXT NOT NULL,
 attempt INTEGER NOT NULL, owner_id TEXT NOT NULL, lease_epoch INTEGER NOT NULL,
 started_at TEXT NOT NULL, finished_at TEXT, outcome TEXT NOT NULL DEFAULT '',
 checkpoint_json TEXT NOT NULL DEFAULT '', result_json TEXT NOT NULL DEFAULT '', error_text TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (tenant_id, repository_id, delivery_id, attempt),
 UNIQUE (tenant_id, repository_id, delivery_id, lease_epoch),
 FOREIGN KEY (tenant_id, repository_id, delivery_id) REFERENCES delivery_deliveries ON DELETE CASCADE
);
INSERT INTO delivery_queue (tenant_id, repository_id, delivery_id, status, available_at, waiting_signal, created_at, updated_at)
 SELECT tenant_id, repository_id, delivery_id,
  CASE WHEN state = 'queued' THEN 'ready' WHEN state IN ('succeeded','failed') THEN 'done' WHEN state IN ('cancel_requested','cancelled') THEN 'cancelled' ELSE 'waiting' END,
  updated_at,
  CASE WHEN state = 'waiting_retry' THEN 'retry' WHEN state IN ('waiting_decision','waiting_credentials','waiting_quota','paused') THEN 'legacy:' || state ELSE '' END,
  created_at, updated_at
 FROM delivery_deliveries
 WHERE state IN ('queued','waiting_retry','waiting_decision','waiting_credentials','waiting_quota','paused','cancel_requested','succeeded','failed','cancelled');
CREATE INDEX idx_delivery_queue_claim ON delivery_queue (status, available_at, lease_expires_at, created_at, delivery_id);`

const deliverySchemaV4 = `
CREATE TABLE delivery_operations (
  tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, delivery_id TEXT NOT NULL,
  operation_id TEXT NOT NULL, effect_key TEXT NOT NULL, kind TEXT NOT NULL,
  replay_class TEXT NOT NULL CHECK (replay_class IN ('read','fingerprinted_write','idempotent_external','unknown')),
  goal_revision INTEGER NOT NULL,
  input_fingerprint TEXT NOT NULL, output_fingerprint TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('prepared','running','observed','reconciled')),
  owner_id TEXT NOT NULL, lease_epoch INTEGER NOT NULL, attempt INTEGER NOT NULL,
  prepared_at TEXT NOT NULL, updated_at TEXT NOT NULL, result_json TEXT NOT NULL DEFAULT '', error_text TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, repository_id, delivery_id, operation_id),
  UNIQUE (tenant_id, repository_id, delivery_id, effect_key),
  FOREIGN KEY (tenant_id, repository_id, delivery_id) REFERENCES delivery_deliveries ON DELETE CASCADE
);
CREATE INDEX idx_delivery_operations_status ON delivery_operations (tenant_id, repository_id, delivery_id, status);`

const deliverySchemaV5 = `
ALTER TABLE delivery_deliveries ADD COLUMN max_cost_microdollars INTEGER NOT NULL DEFAULT 0 CHECK (max_cost_microdollars >= 0);
CREATE TABLE delivery_usage_calls (
  tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, delivery_id TEXT NOT NULL,
  call_id TEXT NOT NULL, status TEXT NOT NULL CHECK (status IN ('reserved','reconciled','unknown','cancelled')),
  provider TEXT NOT NULL, model TEXT NOT NULL, estimate_tokens INTEGER NOT NULL CHECK (estimate_tokens >= 0),
  reserved_microdollars INTEGER NOT NULL CHECK (reserved_microdollars >= 0),
  known_price INTEGER NOT NULL CHECK (known_price IN (0,1)),
  input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  actual_microdollars INTEGER NOT NULL DEFAULT 0, actual_cost_known INTEGER NOT NULL DEFAULT 0 CHECK (actual_cost_known IN (0,1)), active_nanoseconds INTEGER NOT NULL DEFAULT 0,
  prepared_at TEXT NOT NULL, reconciled_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, repository_id, delivery_id, call_id),
  FOREIGN KEY (tenant_id, repository_id, delivery_id) REFERENCES delivery_deliveries ON DELETE CASCADE
);
CREATE INDEX idx_delivery_usage_calls_status ON delivery_usage_calls (tenant_id, repository_id, delivery_id, status);`

const deliverySchemaV6 = `
ALTER TABLE delivery_operations ADD COLUMN arguments_fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE delivery_operations ADD COLUMN observation_path TEXT NOT NULL DEFAULT '';
ALTER TABLE delivery_operations ADD COLUMN input_state_fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE delivery_operations ADD COLUMN expected_output_fingerprint TEXT NOT NULL DEFAULT '';`

const deliverySchemaV7 = `ALTER TABLE delivery_usage_calls ADD COLUMN provider_nanoseconds INTEGER NOT NULL DEFAULT 0 CHECK (provider_nanoseconds >= 0);`

const deliverySchemaV8 = `ALTER TABLE delivery_worker_attempts ADD COLUMN active_nanoseconds INTEGER NOT NULL DEFAULT 0 CHECK (active_nanoseconds >= 0);`
