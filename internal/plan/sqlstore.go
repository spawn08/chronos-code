package plan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

var (
	ErrStaleVersion       = errors.New("stale plan version")
	ErrUnsupportedSchema  = errors.New("unsupported newer plan schema")
	ErrIncompatibleSchema = errors.New("incompatible plan schema")
	ErrInvalidPlanScope   = errors.New("invalid plan scope")
	ErrInvalidPlanRef     = errors.New("invalid plan reference")
)

const schemaVersion = 1

// SQLStore is the SQLite-backed durable plan repository.
type SQLStore struct {
	db   *sql.DB
	path string
}

// PlanScope is the required tenant and repository boundary for plan operations.
type PlanScope struct {
	TenantID     TenantID     `json:"tenant_id"`
	RepositoryID RepositoryID `json:"repository_id"`
}

// PlanRef identifies one immutable plan generation within a PlanScope.
type PlanRef struct {
	TaskID     TaskID       `json:"task_id"`
	PlanID     PlanID       `json:"plan_id"`
	Generation GenerationID `json:"generation_id"`
}

type PlanSummary struct {
	PlanRef
	State      PlanState  `json:"state"`
	StopReason StopReason `json:"stop_reason"`
	Version    int64      `json:"version"`
}

type AttemptRecord struct {
	ID     AttemptID `json:"id"`
	NodeID NodeID    `json:"node_id"`
}

type EventRecord struct {
	ID             EventID        `json:"id"`
	NodeID         NodeID         `json:"node_id"`
	IdempotencyKey IdempotencyKey `json:"idempotency_key"`
}

// PlanView contains only metadata safe to inspect or export.
type PlanView struct {
	PlanSummary
	Nodes        []Node          `json:"nodes"`
	Dependencies []Dependency    `json:"dependencies"`
	Attempts     []AttemptRecord `json:"attempts"`
	ContextRefs  []ContextRef    `json:"context_refs"`
	Evidence     []Evidence      `json:"evidence"`
	Leases       []Lease         `json:"leases"`
	Events       []EventRecord   `json:"events"`
}

type PlanGraph struct {
	PlanRef
	Nodes        []Node       `json:"nodes"`
	Dependencies []Dependency `json:"dependencies"`
}

type IntegrityResult struct {
	SchemaVersion int  `json:"schema_version"`
	Healthy       bool `json:"healthy"`
}

type BackupRequest struct {
	Path string
}

type BackupResult struct {
	Path string `json:"path"`
}

type RestoreRequest struct {
	SourcePath string
	BackupPath string
}

type RestoreResult struct {
	Backup BackupResult `json:"backup"`
}

type PlanExport struct {
	Version int        `json:"version"`
	Scope   PlanScope  `json:"scope"`
	Plans   []PlanView `json:"plans"`
}

type PruneRequest struct {
	Scope  PlanScope
	DryRun bool
}

type PruneResult struct {
	Plans  int  `json:"plans"`
	Rows   int  `json:"rows"`
	DryRun bool `json:"dry_run"`
}

// PlanControlRequest identifies a scoped plan transition and its expected version.
type PlanControlRequest struct {
	Scope           PlanScope
	Ref             PlanRef
	ExpectedVersion int64
}

// RetryRequest identifies the leased node to retry and the durable retry event.
type RetryRequest struct {
	Scope          PlanScope
	Ref            PlanRef
	NodeID         NodeID
	LeaseID        LeaseID
	EventID        EventID
	IdempotencyKey IdempotencyKey
	MaxAttempts    int
}

const redactedIdempotencyKey IdempotencyKey = "[REDACTED]"

// OpenSQLStore opens path, refuses schemas newer than this binary understands,
// and applies the initial schema transactionally.
func OpenSQLStore(ctx context.Context, path string) (*SQLStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open plan store: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &SQLStore{db: db, path: path}
	if err := store.refuseNewerSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure plan store: %w", err)
	}
	if err := store.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLStore) Close() error { return s.db.Close() }

// Migrate applies each schema version and its marker in the same transaction.
func (s *SQLStore) Migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin plan migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS plan_schema_migrations (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create plan migration table: %w", err)
	}
	var latestVersion int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM plan_schema_migrations`).Scan(&latestVersion); err != nil {
		return fmt.Errorf("read latest plan migration: %w", err)
	}
	if latestVersion > schemaVersion {
		return ErrUnsupportedSchema
	}
	var checksum string
	err = tx.QueryRowContext(ctx, `SELECT checksum FROM plan_schema_migrations WHERE version = ?`, schemaVersion).Scan(&checksum)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read plan migration: %w", err)
	}
	const migrationChecksum = "c6f8c0da8c42f04a"
	if err == nil {
		if checksum != migrationChecksum {
			return ErrIncompatibleSchema
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply plan migration: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_schema_migrations (version, checksum) VALUES (?, ?)`, schemaVersion, migrationChecksum); err != nil {
		return fmt.Errorf("record plan migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit plan migration: %w", err)
	}
	return nil
}

func (s *SQLStore) refuseNewerSchema(ctx context.Context) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'plan_schema_migrations')`).Scan(&exists); err != nil {
		return fmt.Errorf("inspect plan schema: %w", err)
	}
	if exists == 0 {
		return nil
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM plan_schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("read plan schema version: %w", err)
	}
	if version > schemaVersion {
		return ErrUnsupportedSchema
	}
	return nil
}

// Create persists a complete immutable generation and its related identities.
func (s *SQLStore) Create(ctx context.Context, p Plan) error {
	if err := p.ValidateDAG(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create plan: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO plans (tenant_id, repository_id, task_id, plan_id, generation_id, state, stop_reason) VALUES (?, ?, ?, ?, ?, ?, ?)`, p.TenantID, p.RepositoryID, p.TaskID, p.ID, p.Generation, p.State, p.StopReason); err != nil {
		return fmt.Errorf("insert plan: %w", err)
	}
	for _, node := range p.Nodes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_nodes (tenant_id, repository_id, task_id, plan_id, generation_id, node_id, state) VALUES (?, ?, ?, ?, ?, ?, ?)`, p.TenantID, p.RepositoryID, p.TaskID, p.ID, p.Generation, node.ID, node.State); err != nil {
			return fmt.Errorf("insert node: %w", err)
		}
	}
	for _, edge := range p.Dependencies {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_edges (tenant_id, repository_id, task_id, plan_id, generation_id, node_id, depends_on) VALUES (?, ?, ?, ?, ?, ?, ?)`, p.TenantID, p.RepositoryID, p.TaskID, p.ID, p.Generation, edge.NodeID, edge.DependsOn); err != nil {
			return fmt.Errorf("insert dependency: %w", err)
		}
	}
	for _, a := range p.Attempts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_attempts (tenant_id, repository_id, task_id, plan_id, generation_id, attempt_id, node_id, idempotency_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, p.TenantID, p.RepositoryID, p.TaskID, p.ID, p.Generation, a.ID, a.NodeID, a.IdempotencyKey); err != nil {
			return fmt.Errorf("insert attempt: %w", err)
		}
	}
	for _, r := range p.ContextRefs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_context_refs (tenant_id, repository_id, task_id, plan_id, generation_id, context_id, node_id) VALUES (?, ?, ?, ?, ?, ?, ?)`, p.TenantID, p.RepositoryID, p.TaskID, p.ID, p.Generation, r.ID, r.NodeID); err != nil {
			return fmt.Errorf("insert context reference: %w", err)
		}
	}
	for _, e := range p.Evidence {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_evidence (tenant_id, repository_id, task_id, plan_id, generation_id, evidence_id, node_id) VALUES (?, ?, ?, ?, ?, ?, ?)`, p.TenantID, p.RepositoryID, p.TaskID, p.ID, p.Generation, e.ID, e.NodeID); err != nil {
			return fmt.Errorf("insert evidence: %w", err)
		}
	}
	for _, e := range p.Events {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_events (tenant_id, repository_id, task_id, plan_id, generation_id, event_id, node_id, idempotency_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, p.TenantID, p.RepositoryID, p.TaskID, p.ID, p.Generation, e.ID, e.NodeID, e.IdempotencyKey); err != nil {
			return fmt.Errorf("insert event: %w", err)
		}
	}
	for _, l := range p.Leases {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_leases (tenant_id, repository_id, task_id, plan_id, generation_id, lease_id, attempt_id) VALUES (?, ?, ?, ?, ?, ?, ?)`, p.TenantID, p.RepositoryID, p.TaskID, p.ID, p.Generation, l.ID, l.AttemptID); err != nil {
			return fmt.Errorf("insert lease: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create plan: %w", err)
	}
	return nil
}

// Version returns the optimistic-concurrency version of p's generation.
func (s *SQLStore) Version(ctx context.Context, p Plan) (int64, error) {
	var version int64
	err := s.db.QueryRowContext(ctx, planWhere(`SELECT version FROM plans`), planArgs(p)...).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read plan version: %w", err)
	}
	return version, nil
}

// Load returns the complete generation selected by p's stable identity.
func (s *SQLStore) Load(ctx context.Context, p Plan) (Plan, error) {
	var loaded Plan
	loaded.TenantID, loaded.RepositoryID, loaded.TaskID, loaded.ID, loaded.Generation = p.TenantID, p.RepositoryID, p.TaskID, p.ID, p.Generation
	if err := s.db.QueryRowContext(ctx, planWhere(`SELECT state, stop_reason FROM plans`), planArgs(p)...).Scan(&loaded.State, &loaded.StopReason); err != nil {
		return Plan{}, fmt.Errorf("load plan: %w", err)
	}
	if err := s.loadNodes(ctx, &loaded); err != nil {
		return Plan{}, err
	}
	if err := s.loadDependencies(ctx, &loaded); err != nil {
		return Plan{}, err
	}
	if err := s.loadRelated(ctx, &loaded); err != nil {
		return Plan{}, err
	}
	return loaded, nil
}

func (s *SQLStore) loadNodes(ctx context.Context, p *Plan) error {
	rows, err := s.db.QueryContext(ctx, planWhere(`SELECT node_id, state FROM plan_nodes`)+` ORDER BY node_id`, planArgs(*p)...)
	if err != nil {
		return fmt.Errorf("load nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var node Node
		if err := rows.Scan(&node.ID, &node.State); err != nil {
			return fmt.Errorf("scan node: %w", err)
		}
		p.Nodes = append(p.Nodes, node)
	}
	return rows.Err()
}

func (s *SQLStore) loadDependencies(ctx context.Context, p *Plan) error {
	rows, err := s.db.QueryContext(ctx, planWhere(`SELECT node_id, depends_on FROM plan_edges`)+` ORDER BY node_id, depends_on`, planArgs(*p)...)
	if err != nil {
		return fmt.Errorf("load dependencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var edge Dependency
		if err := rows.Scan(&edge.NodeID, &edge.DependsOn); err != nil {
			return fmt.Errorf("scan dependency: %w", err)
		}
		p.Dependencies = append(p.Dependencies, edge)
	}
	return rows.Err()
}

func (s *SQLStore) loadRelated(ctx context.Context, p *Plan) error {
	queries := []struct {
		query string
		scan  func(*sql.Rows) error
	}{
		{planWhere(`SELECT attempt_id, node_id, idempotency_key FROM plan_attempts`) + ` ORDER BY attempt_id`, func(rows *sql.Rows) error {
			var x Attempt
			if err := rows.Scan(&x.ID, &x.NodeID, &x.IdempotencyKey); err != nil {
				return err
			}
			p.Attempts = append(p.Attempts, x)
			return nil
		}},
		{planWhere(`SELECT context_id, node_id FROM plan_context_refs`) + ` ORDER BY context_id`, func(rows *sql.Rows) error {
			var x ContextRef
			if err := rows.Scan(&x.ID, &x.NodeID); err != nil {
				return err
			}
			p.ContextRefs = append(p.ContextRefs, x)
			return nil
		}},
		{planWhere(`SELECT evidence_id, node_id FROM plan_evidence`) + ` ORDER BY evidence_id`, func(rows *sql.Rows) error {
			var x Evidence
			if err := rows.Scan(&x.ID, &x.NodeID); err != nil {
				return err
			}
			p.Evidence = append(p.Evidence, x)
			return nil
		}},
		{planWhere(`SELECT lease_id, attempt_id FROM plan_leases`) + ` ORDER BY lease_id`, func(rows *sql.Rows) error {
			var x Lease
			if err := rows.Scan(&x.ID, &x.AttemptID); err != nil {
				return err
			}
			p.Leases = append(p.Leases, x)
			return nil
		}},
		{planWhere(`SELECT event_id, node_id, idempotency_key FROM plan_events`) + ` ORDER BY sequence`, func(rows *sql.Rows) error {
			var x Event
			if err := rows.Scan(&x.ID, &x.NodeID, &x.IdempotencyKey); err != nil {
				return err
			}
			p.Events = append(p.Events, x)
			return nil
		}},
	}
	for _, item := range queries {
		rows, err := s.db.QueryContext(ctx, item.query, planArgs(*p)...)
		if err != nil {
			return fmt.Errorf("load plan related rows: %w", err)
		}
		for rows.Next() {
			if err := item.scan(rows); err != nil {
				rows.Close()
				return fmt.Errorf("scan plan related row: %w", err)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("load plan related rows: %w", err)
		}
		rows.Close()
	}
	return nil
}

// TransitionPlan changes state only when expectedVersion is current.
func (s *SQLStore) TransitionPlan(ctx context.Context, p Plan, expectedVersion int64, next PlanState) error {
	current, err := s.Load(ctx, p)
	if err != nil {
		return err
	}
	if err := current.Transition(next); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, planWhere(`UPDATE plans SET state = ?, version = version + 1`)+` AND version = ?`, append([]any{next}, append(planArgs(p), expectedVersion)...)...)
	if err != nil {
		return fmt.Errorf("transition plan: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count plan transition: %w", err)
	}
	if changed != 1 {
		return ErrStaleVersion
	}
	return nil
}

// TransitionNode changes node state only when the generation version matches.
func (s *SQLStore) TransitionNode(ctx context.Context, p Plan, node Node, expectedVersion int64, next NodeState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin node transition: %w", err)
	}
	defer tx.Rollback()
	var state NodeState
	if err := tx.QueryRowContext(ctx, `SELECT n.state FROM plan_nodes n JOIN plans p ON p.tenant_id = n.tenant_id AND p.repository_id = n.repository_id AND p.task_id = n.task_id AND p.plan_id = n.plan_id AND p.generation_id = n.generation_id WHERE p.tenant_id = ? AND p.repository_id = ? AND p.task_id = ? AND p.plan_id = ? AND p.generation_id = ? AND n.node_id = ?`, append(planArgs(p), node.ID)...).Scan(&state); err != nil {
		return fmt.Errorf("read node state: %w", err)
	}
	if err := (&Node{State: state}).Transition(next); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, planWhere(`UPDATE plans SET version = version + 1`)+` AND version = ?`, append(planArgs(p), expectedVersion)...)
	if err != nil {
		return fmt.Errorf("advance plan version: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count node transition: %w", err)
	}
	if changed != 1 {
		return ErrStaleVersion
	}
	if _, err := tx.ExecContext(ctx, planWhere(`UPDATE plan_nodes SET state = ?`)+` AND node_id = ?`, append([]any{next}, append(planArgs(p), node.ID)...)...); err != nil {
		return fmt.Errorf("transition node: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit node transition: %w", err)
	}
	return nil
}

// List returns plan generations only from scope.
func (s *SQLStore) List(ctx context.Context, scope PlanScope) ([]PlanSummary, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, plan_id, generation_id, state, stop_reason, version FROM plans WHERE tenant_id = ? AND repository_id = ? ORDER BY task_id, plan_id, generation_id`, scope.TenantID, scope.RepositoryID)
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	defer rows.Close()
	var plans []PlanSummary
	for rows.Next() {
		var plan PlanSummary
		if err := rows.Scan(&plan.TaskID, &plan.PlanID, &plan.Generation, &plan.State, &plan.StopReason, &plan.Version); err != nil {
			return nil, fmt.Errorf("scan listed plan: %w", err)
		}
		plans = append(plans, plan)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	return plans, nil
}

// Show returns a redacted plan generation from scope.
func (s *SQLStore) Show(ctx context.Context, scope PlanScope, ref PlanRef) (PlanView, error) {
	p, err := scopedPlan(scope, ref)
	if err != nil {
		return PlanView{}, err
	}
	loaded, err := s.Load(ctx, p)
	if err != nil {
		return PlanView{}, err
	}
	version, err := s.Version(ctx, p)
	if err != nil {
		return PlanView{}, err
	}
	return planView(loaded, version), nil
}

// Graph returns topology from one scoped generation.
func (s *SQLStore) Graph(ctx context.Context, scope PlanScope, ref PlanRef) (PlanGraph, error) {
	p, err := scopedPlan(scope, ref)
	if err != nil {
		return PlanGraph{}, err
	}
	loaded, err := s.Load(ctx, p)
	if err != nil {
		return PlanGraph{}, err
	}
	return PlanGraph{PlanRef: ref, Nodes: loaded.Nodes, Dependencies: loaded.Dependencies}, nil
}

// Events returns redacted transition events from one scoped generation.
func (s *SQLStore) Events(ctx context.Context, scope PlanScope, ref PlanRef) ([]EventRecord, error) {
	p, err := scopedPlan(scope, ref)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, planWhere(`SELECT event_id, node_id FROM plan_events`)+` ORDER BY sequence`, planArgs(p)...)
	if err != nil {
		return nil, fmt.Errorf("list plan events: %w", err)
	}
	defer rows.Close()
	var events []EventRecord
	for rows.Next() {
		var event EventRecord
		if err := rows.Scan(&event.ID, &event.NodeID); err != nil {
			return nil, fmt.Errorf("scan plan event: %w", err)
		}
		event.IdempotencyKey = redactedIdempotencyKey
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list plan events: %w", err)
	}
	return events, nil
}

// Resume transitions a paused plan to active when the expected version is current.
func (s *SQLStore) Resume(ctx context.Context, request PlanControlRequest) (PlanSummary, error) {
	return s.controlPlan(ctx, request, PlanActive)
}

// Pause transitions an active plan to paused when the expected version is current.
func (s *SQLStore) Pause(ctx context.Context, request PlanControlRequest) (PlanSummary, error) {
	return s.controlPlan(ctx, request, PlanPaused)
}

// Cancel transitions a non-terminal plan to canceled when the expected version is current.
func (s *SQLStore) Cancel(ctx context.Context, request PlanControlRequest) (PlanSummary, error) {
	return s.controlPlan(ctx, request, PlanCanceled)
}

// Retry transitions a leased node using the scheduler's lease and attempt semantics.
func (s *SQLStore) Retry(ctx context.Context, request RetryRequest) error {
	p, err := scopedPlan(request.Scope, request.Ref)
	if err != nil {
		return err
	}
	if request.NodeID == "" || request.LeaseID == "" || request.EventID == "" || request.IdempotencyKey == "" {
		return fmt.Errorf("retry plan node: missing durable identity")
	}
	return NewScheduler(s, SchedulerConfig{MaxAttempts: request.MaxAttempts}).Retry(ctx, p, request.NodeID, request.LeaseID, request.EventID, request.IdempotencyKey)
}

func (s *SQLStore) controlPlan(ctx context.Context, request PlanControlRequest, next PlanState) (PlanSummary, error) {
	p, err := scopedPlan(request.Scope, request.Ref)
	if err != nil {
		return PlanSummary{}, err
	}
	if err := s.TransitionPlan(ctx, p, request.ExpectedVersion, next); err != nil {
		return PlanSummary{}, err
	}
	view, err := s.Show(ctx, request.Scope, request.Ref)
	if err != nil {
		return PlanSummary{}, err
	}
	return view.PlanSummary, nil
}

// Integrity checks the database and verifies the supported schema marker.
func (s *SQLStore) Integrity(ctx context.Context) (IntegrityResult, error) {
	version, err := validatePlanDatabase(ctx, s.db)
	if err != nil {
		return IntegrityResult{}, err
	}
	return IntegrityResult{SchemaVersion: version, Healthy: true}, nil
}

// Backup writes a consistent database snapshot to a new path.
func (s *SQLStore) Backup(ctx context.Context, request BackupRequest) (BackupResult, error) {
	if request.Path == "" {
		return BackupResult{}, fmt.Errorf("backup plan store: missing path")
	}
	if samePath(s.path, request.Path) {
		return BackupResult{}, fmt.Errorf("backup plan store: destination is source database")
	}
	if _, err := os.Stat(request.Path); err == nil {
		return BackupResult{}, fmt.Errorf("backup plan store: destination exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return BackupResult{}, fmt.Errorf("inspect backup destination: %w", err)
	}
	if _, err := s.Integrity(ctx); err != nil {
		return BackupResult{}, fmt.Errorf("backup plan store: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, request.Path); err != nil {
		return BackupResult{}, fmt.Errorf("backup plan store: %w", err)
	}
	return BackupResult{Path: request.Path}, nil
}

// Restore validates and snapshots source before backing up and replacing this store.
func (s *SQLStore) Restore(ctx context.Context, request RestoreRequest) (RestoreResult, error) {
	if request.SourcePath == "" || request.BackupPath == "" {
		return RestoreResult{}, fmt.Errorf("restore plan store: source and backup paths are required")
	}
	if samePath(s.path, request.SourcePath) || samePath(s.path, request.BackupPath) || samePath(request.SourcePath, request.BackupPath) {
		return RestoreResult{}, fmt.Errorf("restore plan store: source, backup, and destination must differ")
	}
	if _, err := os.Stat(request.SourcePath); err != nil {
		return RestoreResult{}, fmt.Errorf("inspect restore source: %w", err)
	}
	source, err := sql.Open("sqlite", request.SourcePath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("open restore source: %w", err)
	}
	source.SetMaxOpenConns(1)
	version, err := validatePlanDatabase(ctx, source)
	if err != nil {
		source.Close()
		return RestoreResult{}, fmt.Errorf("validate restore source: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".plans-restore-*.db")
	if err != nil {
		source.Close()
		return RestoreResult{}, fmt.Errorf("create restore snapshot: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		source.Close()
		return RestoreResult{}, fmt.Errorf("close restore snapshot: %w", err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		source.Close()
		return RestoreResult{}, fmt.Errorf("prepare restore snapshot: %w", err)
	}
	defer os.Remove(temporaryPath)
	if _, err := source.ExecContext(ctx, `VACUUM INTO ?`, temporaryPath); err != nil {
		source.Close()
		return RestoreResult{}, fmt.Errorf("snapshot restore source: %w", err)
	}
	if err := source.Close(); err != nil {
		return RestoreResult{}, fmt.Errorf("close restore source: %w", err)
	}
	if version != schemaVersion {
		return RestoreResult{}, ErrIncompatibleSchema
	}
	backup, err := s.Backup(ctx, BackupRequest{Path: request.BackupPath})
	if err != nil {
		return RestoreResult{}, err
	}
	if err := s.db.Close(); err != nil {
		return RestoreResult{}, fmt.Errorf("close plan store before restore: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return RestoreResult{}, fmt.Errorf("replace plan store: %w", err)
	}
	reopened, err := OpenSQLStore(ctx, s.path)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("reopen restored plan store: %w", err)
	}
	s.db = reopened.db
	return RestoreResult{Backup: backup}, nil
}

// Export returns all redacted plan records within an explicit scope.
func (s *SQLStore) Export(ctx context.Context, scope PlanScope) (PlanExport, error) {
	plans, err := s.List(ctx, scope)
	if err != nil {
		return PlanExport{}, err
	}
	exported := PlanExport{Version: 1, Scope: scope}
	for _, summary := range plans {
		view, err := s.Show(ctx, scope, summary.PlanRef)
		if err != nil {
			return PlanExport{}, err
		}
		exported.Plans = append(exported.Plans, view)
	}
	return exported, nil
}

// Prune removes every plan record within an explicit scope, or reports its effect.
func (s *SQLStore) Prune(ctx context.Context, request PruneRequest) (PruneResult, error) {
	if err := request.Scope.Validate(); err != nil {
		return PruneResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PruneResult{}, fmt.Errorf("begin prune plans: %w", err)
	}
	defer tx.Rollback()
	result := PruneResult{DryRun: request.DryRun}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plans WHERE tenant_id = ? AND repository_id = ?`, request.Scope.TenantID, request.Scope.RepositoryID).Scan(&result.Plans); err != nil {
		return PruneResult{}, fmt.Errorf("count pruned plans: %w", err)
	}
	for _, table := range []string{"plan_leases", "plan_events", "plan_evidence", "plan_context_refs", "plan_attempts", "plan_edges", "plan_nodes", "plans"} {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE tenant_id = ? AND repository_id = ?`, request.Scope.TenantID, request.Scope.RepositoryID).Scan(&count); err != nil {
			return PruneResult{}, fmt.Errorf("count pruned %s: %w", table, err)
		}
		result.Rows += count
		if !request.DryRun {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE tenant_id = ? AND repository_id = ?`, request.Scope.TenantID, request.Scope.RepositoryID); err != nil {
				return PruneResult{}, fmt.Errorf("prune %s: %w", table, err)
			}
		}
	}
	if request.DryRun {
		return result, nil
	}
	if err := tx.Commit(); err != nil {
		return PruneResult{}, fmt.Errorf("commit prune plans: %w", err)
	}
	return result, nil
}

func (scope PlanScope) Validate() error {
	if scope.TenantID == "" || scope.RepositoryID == "" {
		return ErrInvalidPlanScope
	}
	return nil
}

func scopedPlan(scope PlanScope, ref PlanRef) (Plan, error) {
	if err := scope.Validate(); err != nil {
		return Plan{}, err
	}
	if ref.TaskID == "" || ref.PlanID == "" || ref.Generation == "" {
		return Plan{}, ErrInvalidPlanRef
	}
	return Plan{TenantID: scope.TenantID, RepositoryID: scope.RepositoryID, TaskID: ref.TaskID, ID: ref.PlanID, Generation: ref.Generation}, nil
}

func planView(p Plan, version int64) PlanView {
	view := PlanView{PlanSummary: PlanSummary{PlanRef: PlanRef{TaskID: p.TaskID, PlanID: p.ID, Generation: p.Generation}, State: p.State, StopReason: p.StopReason, Version: version}, Nodes: p.Nodes, Dependencies: p.Dependencies, ContextRefs: p.ContextRefs, Evidence: p.Evidence, Leases: p.Leases}
	for _, attempt := range p.Attempts {
		view.Attempts = append(view.Attempts, AttemptRecord{ID: attempt.ID, NodeID: attempt.NodeID})
	}
	for _, event := range p.Events {
		view.Events = append(view.Events, EventRecord{ID: event.ID, NodeID: event.NodeID, IdempotencyKey: redactedIdempotencyKey})
	}
	return view
}

func validatePlanDatabase(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return 0, fmt.Errorf("run plan database integrity check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return 0, fmt.Errorf("scan plan database integrity check: %w", err)
		}
		if result != "ok" {
			return 0, fmt.Errorf("plan database integrity check: %s", result)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("run plan database integrity check: %w", err)
	}
	var version int
	var checksum string
	if err := db.QueryRowContext(ctx, `SELECT version, checksum FROM plan_schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &checksum); err != nil {
		return 0, fmt.Errorf("read plan database schema: %w", err)
	}
	if version > schemaVersion {
		return 0, ErrUnsupportedSchema
	}
	if version != schemaVersion || checksum != "c6f8c0da8c42f04a" {
		return 0, ErrIncompatibleSchema
	}
	for _, table := range []string{"plans", "plan_nodes", "plan_edges", "plan_attempts", "plan_context_refs", "plan_evidence", "plan_events", "plan_leases"} {
		var exists int
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)`, table).Scan(&exists); err != nil {
			return 0, fmt.Errorf("inspect plan database table %s: %w", table, err)
		}
		if exists == 0 {
			return 0, ErrIncompatibleSchema
		}
	}
	return version, nil
}

func samePath(first, second string) bool {
	firstPath, firstErr := filepath.Abs(first)
	secondPath, secondErr := filepath.Abs(second)
	return firstErr == nil && secondErr == nil && firstPath == secondPath
}

func planWhere(prefix string) string {
	return prefix + ` WHERE tenant_id = ? AND repository_id = ? AND task_id = ? AND plan_id = ? AND generation_id = ?`
}
func planArgs(p Plan) []any { return []any{p.TenantID, p.RepositoryID, p.TaskID, p.ID, p.Generation} }

const schemaSQL = `
CREATE TABLE plans (tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, task_id TEXT NOT NULL, plan_id TEXT NOT NULL, generation_id TEXT NOT NULL, state TEXT NOT NULL, stop_reason TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1, PRIMARY KEY (tenant_id, repository_id, task_id, plan_id, generation_id));
CREATE TABLE plan_nodes (tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, task_id TEXT NOT NULL, plan_id TEXT NOT NULL, generation_id TEXT NOT NULL, node_id TEXT NOT NULL, state TEXT NOT NULL, PRIMARY KEY (tenant_id, repository_id, task_id, plan_id, generation_id, node_id), FOREIGN KEY (tenant_id, repository_id, task_id, plan_id, generation_id) REFERENCES plans);
CREATE TABLE plan_edges (tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, task_id TEXT NOT NULL, plan_id TEXT NOT NULL, generation_id TEXT NOT NULL, node_id TEXT NOT NULL, depends_on TEXT NOT NULL, PRIMARY KEY (tenant_id, repository_id, task_id, plan_id, generation_id, node_id, depends_on), FOREIGN KEY (tenant_id, repository_id, task_id, plan_id, generation_id, node_id) REFERENCES plan_nodes, FOREIGN KEY (tenant_id, repository_id, task_id, plan_id, generation_id, depends_on) REFERENCES plan_nodes);
CREATE TABLE plan_attempts (tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, task_id TEXT NOT NULL, plan_id TEXT NOT NULL, generation_id TEXT NOT NULL, attempt_id TEXT NOT NULL, node_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, PRIMARY KEY (tenant_id, repository_id, task_id, plan_id, generation_id, attempt_id), FOREIGN KEY (tenant_id, repository_id, task_id, plan_id, generation_id, node_id) REFERENCES plan_nodes);
CREATE TABLE plan_context_refs (tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, task_id TEXT NOT NULL, plan_id TEXT NOT NULL, generation_id TEXT NOT NULL, context_id TEXT NOT NULL, node_id TEXT NOT NULL, PRIMARY KEY (tenant_id, repository_id, task_id, plan_id, generation_id, context_id), FOREIGN KEY (tenant_id, repository_id, task_id, plan_id, generation_id, node_id) REFERENCES plan_nodes);
CREATE TABLE plan_evidence (tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, task_id TEXT NOT NULL, plan_id TEXT NOT NULL, generation_id TEXT NOT NULL, evidence_id TEXT NOT NULL, node_id TEXT NOT NULL, PRIMARY KEY (tenant_id, repository_id, task_id, plan_id, generation_id, evidence_id), FOREIGN KEY (tenant_id, repository_id, task_id, plan_id, generation_id, node_id) REFERENCES plan_nodes);
CREATE TABLE plan_events (sequence INTEGER PRIMARY KEY AUTOINCREMENT, tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, task_id TEXT NOT NULL, plan_id TEXT NOT NULL, generation_id TEXT NOT NULL, event_id TEXT NOT NULL, node_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, UNIQUE (tenant_id, repository_id, task_id, plan_id, generation_id, event_id), FOREIGN KEY (tenant_id, repository_id, task_id, plan_id, generation_id, node_id) REFERENCES plan_nodes);
CREATE TABLE plan_leases (tenant_id TEXT NOT NULL, repository_id TEXT NOT NULL, task_id TEXT NOT NULL, plan_id TEXT NOT NULL, generation_id TEXT NOT NULL, lease_id TEXT NOT NULL, attempt_id TEXT NOT NULL, PRIMARY KEY (tenant_id, repository_id, task_id, plan_id, generation_id, lease_id), FOREIGN KEY (tenant_id, repository_id, task_id, plan_id, generation_id, attempt_id) REFERENCES plan_attempts);
CREATE INDEX idx_plan_nodes_state ON plan_nodes (tenant_id, repository_id, state);
CREATE INDEX idx_plan_edges_dependency ON plan_edges (tenant_id, repository_id, task_id, plan_id, generation_id, depends_on);`
