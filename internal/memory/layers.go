package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spawn08/chronos/storage"
	_ "modernc.org/sqlite"
)

// Kind describes the meaning of a layered memory, independently of its scope.
type Kind string

const (
	KindEpisodic       Kind = "episodic"       // Completed task outcomes/history, never automatic instructions.
	KindProcedural     Kind = "procedural"     // Explicit reusable steps.
	KindSemantic       Kind = "semantic"       // Optional facts; no embeddings required.
	KindOrganizational Kind = "organizational" // Explicitly published, curated shared standards.
)

// Scope is an exact partition, not a hierarchy or a wildcard. Every partition
// includes the context tenant. User and organization partitions intentionally
// span projects; project partitions do not span users' other projects.
type Scope string

const (
	ScopeTenant       Scope = "tenant"
	ScopeProject      Scope = "project"
	ScopeUser         Scope = "user"
	ScopeOrganization Scope = "organization"
)

const (
	MaxLayerContentBytes  = 8 * 1024 // Combined content and procedural steps, before JSON encoding.
	MaxLayerRecords       = 100
	MaxLayerRecallBytes   = 64 * 1024
	maxLayerMetadataBytes = 512
)

// RetrievalBudget caps the complete JSON record array, including provenance.
// Zero fields select defaults (10 records, 16 KiB); negative values are invalid.
type RetrievalBudget struct {
	MaxRecords int `json:"max_records,omitempty"`
	MaxBytes   int `json:"max_bytes,omitempty"`
}

// LayerOptions is trusted caller configuration, copied by LayerTools. Identity
// fields must come from the coordinator, not model arguments. Tenant identity
// always comes from storage.TenantFromContext at invocation time. The zero value
// disables semantic memory and permits only explicitly selected tenant scope.
type LayerOptions struct {
	ProjectID       string          `json:"project_id,omitempty"`
	UserID          string          `json:"user_id,omitempty"`
	OrganizationID  string          `json:"organization_id,omitempty"`
	SessionID       string          `json:"session_id,omitempty"`
	SemanticEnabled bool            `json:"semantic_enabled"`
	Budget          RetrievalBudget `json:"budget,omitempty"`
}

type Provenance struct {
	SessionID   string     `json:"session_id"`
	Source      string     `json:"source"`
	Revision    string     `json:"revision"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Invalidated bool       `json:"invalidated"`
}

// LayerRecord is deliberately separate from the legacy YAML Record API.
// Records are data: callers must not promote episodic content to instructions.
type LayerRecord struct {
	ID             string     `json:"id"`
	Kind           Kind       `json:"kind"`
	Scope          Scope      `json:"scope"`
	TenantID       string     `json:"tenant_id"`
	ProjectID      string     `json:"project_id,omitempty"`
	UserID         string     `json:"user_id,omitempty"`
	OrganizationID string     `json:"organization_id,omitempty"`
	Content        string     `json:"content"`
	Steps          []string   `json:"steps,omitempty"`
	Published      bool       `json:"published"`
	Provenance     Provenance `json:"provenance"`
}

// LayerRecordInput creates a new immutable entry. Forget invalidates it;
// corrections are new entries with their own source revision and timestamps.
type LayerRecordInput struct {
	Kind      Kind       `json:"kind"`
	Scope     Scope      `json:"scope"`
	Content   string     `json:"content"`
	Steps     []string   `json:"steps,omitempty"`
	Source    string     `json:"source"`
	Revision  string     `json:"revision"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Publish   bool       `json:"publish,omitempty"`
}

// LayerQuery selects exactly one scope. Empty Kind includes enabled kinds;
// empty Query lists recent entries. Limit/MaxBytes may lower the options budget.
type LayerQuery struct {
	Scope    Scope  `json:"scope"`
	Kind     Kind   `json:"kind,omitempty"`
	Query    string `json:"query,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

// ErrLayerNotFound also covers inaccessible, expired, or invalidated entries,
// so forgetting a guessed ID cannot disclose another partition's existence.
var ErrLayerNotFound = errors.New("layer memory: record not found")

// LayerStore owns one SQLite connection. Share this handle between agents and
// close it once at coordinator shutdown. Separate handles may share a DB path.
type LayerStore struct {
	db *sql.DB
}

// OpenLayerStore opens/creates path; its parent directory must already exist.
// Schema and FTS creation are atomic and never rebuild existing indexes on open.
func OpenLayerStore(ctx context.Context, path string) (*LayerStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("layer memory: database path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("layer memory: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("layer memory: configure: %w", err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("layer memory: begin schema: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS layer_memories (
			row_id INTEGER PRIMARY KEY,
			id TEXT NOT NULL UNIQUE,
			tenant_id TEXT NOT NULL, scope TEXT NOT NULL, scope_id TEXT NOT NULL,
			kind TEXT NOT NULL, content TEXT NOT NULL, steps TEXT NOT NULL,
			published INTEGER NOT NULL,
			session_id TEXT NOT NULL, source TEXT NOT NULL, revision TEXT NOT NULL,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			expires_at INTEGER, invalidated INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS layer_memories_scope
			ON layer_memories(tenant_id, scope, scope_id, invalidated, created_at);
		CREATE VIRTUAL TABLE IF NOT EXISTS layer_memories_fts USING fts5(
			content, steps, content='layer_memories', content_rowid='row_id'
		);
		CREATE TRIGGER IF NOT EXISTS layer_memories_insert AFTER INSERT ON layer_memories BEGIN
			INSERT INTO layer_memories_fts(rowid, content, steps) VALUES(new.row_id, new.content, new.steps);
		END;
	`)
	if err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("layer memory: create schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("layer memory: commit schema: %w", err)
	}
	return &LayerStore{db: db}, nil
}

func (s *LayerStore) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("layer memory: close: %w", err)
	}
	return nil
}

func layerText(label, value string, maxBytes int, required bool) error {
	if !utf8.ValidString(value) || len(value) > maxBytes || (required && strings.TrimSpace(value) == "") || strings.ContainsRune(value, 0) {
		return fmt.Errorf("layer memory: invalid %s (maximum %d UTF-8 bytes)", label, maxBytes)
	}
	return nil
}

func normalizeLayerOptions(o LayerOptions) (LayerOptions, error) {
	for _, field := range []struct{ name, value string }{
		{"project_id", o.ProjectID}, {"user_id", o.UserID},
		{"organization_id", o.OrganizationID}, {"session_id", o.SessionID},
	} {
		if err := layerText(field.name, field.value, maxLayerMetadataBytes, field.value != ""); err != nil {
			return o, err
		}
	}
	if o.Budget.MaxRecords == 0 {
		o.Budget.MaxRecords = 10
	}
	if o.Budget.MaxBytes == 0 {
		o.Budget.MaxBytes = 16 * 1024
	}
	if o.Budget.MaxRecords < 1 || o.Budget.MaxRecords > MaxLayerRecords || o.Budget.MaxBytes < 2 || o.Budget.MaxBytes > MaxLayerRecallBytes {
		return o, fmt.Errorf("layer memory: budget requires 1..%d records and 2..%d bytes", MaxLayerRecords, MaxLayerRecallBytes)
	}
	return o, nil
}

func layerPartition(ctx context.Context, o LayerOptions, scope Scope) (tenant, id string, err error) {
	tenant = storage.TenantFromContext(ctx)
	if err = layerText("tenant_id", tenant, maxLayerMetadataBytes, true); err != nil {
		return
	}
	switch scope {
	case ScopeTenant:
		return tenant, "", nil
	case ScopeProject:
		id = o.ProjectID
	case ScopeUser:
		id = o.UserID
	case ScopeOrganization:
		id = o.OrganizationID
	default:
		return "", "", fmt.Errorf("layer memory: invalid scope %q", scope)
	}
	err = layerText(string(scope)+" identity", id, maxLayerMetadataBytes, true)
	return
}

func validateLayerKind(kind Kind, semantic bool) error {
	switch kind {
	case KindEpisodic, KindProcedural, KindOrganizational:
		return nil
	case KindSemantic:
		if semantic {
			return nil
		}
		return fmt.Errorf("layer memory: semantic memory is disabled")
	default:
		return fmt.Errorf("layer memory: invalid kind %q", kind)
	}
}

func setLayerIdentity(r *LayerRecord, id string) {
	switch r.Scope {
	case ScopeProject:
		r.ProjectID = id
	case ScopeUser:
		r.UserID = id
	case ScopeOrganization:
		r.OrganizationID = id
	}
}

// Record validates and persists an explicit memory. It never derives procedures
// from episodes or learns/publishes organization standards automatically.
func (s *LayerStore) Record(ctx context.Context, options LayerOptions, input LayerRecordInput) (LayerRecord, error) {
	o, err := normalizeLayerOptions(options)
	if err != nil {
		return LayerRecord{}, err
	}
	tenant, scopeID, err := layerPartition(ctx, o, input.Scope)
	if err != nil {
		return LayerRecord{}, err
	}
	if err := validateLayerKind(input.Kind, o.SemanticEnabled); err != nil {
		return LayerRecord{}, err
	}
	// Organizational standards are only meaningful in an explicitly configured,
	// published organization partition; other kinds can also be published there.
	if input.Kind == KindOrganizational && input.Scope != ScopeOrganization {
		return LayerRecord{}, fmt.Errorf("layer memory: organizational kind requires organization scope")
	}
	if input.Publish != (input.Scope == ScopeOrganization) {
		return LayerRecord{}, fmt.Errorf("layer memory: organization scope requires explicit publication; other scopes cannot publish")
	}
	if err := layerText("content", input.Content, MaxLayerContentBytes, true); err != nil {
		return LayerRecord{}, err
	}
	if (input.Kind == KindProcedural) != (len(input.Steps) > 0) || len(input.Steps) > 64 {
		return LayerRecord{}, fmt.Errorf("layer memory: procedural kind requires 1..64 explicit steps; other kinds cannot contain steps")
	}
	size := len(input.Content)
	for _, step := range input.Steps {
		if err := layerText("step", step, MaxLayerContentBytes, true); err != nil {
			return LayerRecord{}, err
		}
		size += len(step)
	}
	if size > MaxLayerContentBytes {
		return LayerRecord{}, fmt.Errorf("layer memory: content and steps exceed %d bytes", MaxLayerContentBytes)
	}
	for _, field := range []struct{ name, value string }{
		{"session_id", o.SessionID}, {"source", input.Source}, {"revision", input.Revision},
	} {
		if err := layerText(field.name, field.value, maxLayerMetadataBytes, true); err != nil {
			return LayerRecord{}, err
		}
	}
	var expiry any
	var expiresAt *time.Time
	if input.ExpiresAt != nil {
		t := input.ExpiresAt.UTC()
		// UnixNano is the storage format; reject dates outside its exact range.
		if !time.Unix(0, t.UnixNano()).Equal(t) {
			return LayerRecord{}, fmt.Errorf("layer memory: expiry outside supported timestamp range")
		}
		expiresAt, expiry = &t, t.UnixNano()
	}
	id, err := newID()
	if err != nil {
		return LayerRecord{}, err
	}
	now := time.Now().UTC()
	r := LayerRecord{
		ID: id, Kind: input.Kind, Scope: input.Scope, TenantID: tenant,
		Content: input.Content, Steps: append([]string{}, input.Steps...), Published: input.Publish,
		Provenance: Provenance{SessionID: o.SessionID, Source: input.Source, Revision: input.Revision,
			CreatedAt: now, UpdatedAt: now, ExpiresAt: expiresAt},
	}
	setLayerIdentity(&r, scopeID)
	steps, err := json.Marshal(r.Steps)
	if err != nil {
		return LayerRecord{}, fmt.Errorf("layer memory: encode steps: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO layer_memories
		(id, tenant_id, scope, scope_id, kind, content, steps, published, session_id, source, revision, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, tenant, input.Scope, scopeID, input.Kind, r.Content, string(steps), r.Published,
		o.SessionID, input.Source, input.Revision, now.UnixNano(), now.UnixNano(), expiry)
	if err != nil {
		return LayerRecord{}, fmt.Errorf("layer memory: record: %w", err)
	}
	return r, nil
}

// Recall returns active entries in deterministic FTS5 BM25 order, then newest
// creation time and ID. Query is literal Unicode words (AND), never FTS syntax.
// With no words a nonempty query matches nothing. Whole records are returned;
// the result is a ranked prefix fitting both the record and JSON byte budgets.
func (s *LayerStore) Recall(ctx context.Context, options LayerOptions, query LayerQuery) ([]LayerRecord, error) {
	o, err := normalizeLayerOptions(options)
	if err != nil {
		return nil, err
	}
	tenant, scopeID, err := layerPartition(ctx, o, query.Scope)
	if err != nil {
		return nil, err
	}
	if query.Kind != "" {
		if err := validateLayerKind(query.Kind, o.SemanticEnabled); err != nil {
			return nil, err
		}
	}
	if err := layerText("query", query.Query, 1024, false); err != nil {
		return nil, err
	}
	if query.Limit < 0 || query.MaxBytes < 0 || (query.MaxBytes > 0 && query.MaxBytes < 2) {
		return nil, fmt.Errorf("layer memory: invalid recall budget")
	}
	limit, maxBytes := o.Budget.MaxRecords, o.Budget.MaxBytes
	if query.Limit > 0 {
		limit = min(limit, query.Limit)
	}
	if query.MaxBytes > 0 {
		maxBytes = min(maxBytes, query.MaxBytes)
	}
	result := make([]LayerRecord, 0)
	join, match, order := "", "", "m.created_at DESC, m.id ASC"
	args := []any{tenant, query.Scope, scopeID, time.Now().UnixNano()}
	if strings.TrimSpace(query.Query) != "" {
		words := strings.FieldsFunc(query.Query, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
		if len(words) == 0 {
			return result, nil
		}
		for i := range words {
			words[i] = `"` + words[i] + `"`
		}
		join = " JOIN layer_memories_fts ON layer_memories_fts.rowid = m.row_id"
		match = " AND layer_memories_fts MATCH ?"
		order = "bm25(layer_memories_fts), " + order
		args = append(args, strings.Join(words, " AND "))
	}
	if !o.SemanticEnabled {
		match += " AND m.kind != 'semantic'"
	}
	if query.Kind != "" {
		match += " AND m.kind = ?"
		args = append(args, query.Kind)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.kind, m.content, m.steps, m.published,
		m.session_id, m.source, m.revision, m.created_at, m.updated_at, m.expires_at, m.invalidated
		FROM layer_memories m`+join+`
		WHERE m.tenant_id = ? AND m.scope = ? AND m.scope_id = ?
		AND m.invalidated = 0 AND (m.expires_at IS NULL OR m.expires_at > ?)`+match+`
		ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("layer memory: recall: %w", err)
	}
	defer rows.Close()
	size := 2 // JSON array brackets.
	for rows.Next() {
		r := LayerRecord{Scope: query.Scope, TenantID: tenant}
		setLayerIdentity(&r, scopeID)
		var steps string
		var created, updated int64
		var expiry sql.NullInt64
		p := &r.Provenance
		if err := rows.Scan(&r.ID, &r.Kind, &r.Content, &steps, &r.Published,
			&p.SessionID, &p.Source, &p.Revision, &created, &updated, &expiry, &p.Invalidated); err != nil {
			return nil, fmt.Errorf("layer memory: scan: %w", err)
		}
		p.CreatedAt, p.UpdatedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
		if expiry.Valid {
			t := time.Unix(0, expiry.Int64).UTC()
			p.ExpiresAt = &t
		}
		if err := json.Unmarshal([]byte(steps), &r.Steps); err != nil {
			return nil, fmt.Errorf("layer memory: decode steps: %w", err)
		}
		encoded, err := json.Marshal(r)
		if err != nil {
			return nil, fmt.Errorf("layer memory: encode result: %w", err)
		}
		extra := len(encoded)
		if len(result) > 0 {
			extra++ // Comma between records.
		}
		if size+extra > maxBytes {
			break
		}
		size += extra
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("layer memory: iterate: %w", err)
	}
	return result, nil
}

// Forget soft-invalidates an active entry in the exact scope, retaining its
// provenance for audit. Disabled semantic records cannot be recalled or mutated.
func (s *LayerStore) Forget(ctx context.Context, options LayerOptions, scope Scope, id string) error {
	o, err := normalizeLayerOptions(options)
	if err != nil {
		return err
	}
	tenant, scopeID, err := layerPartition(ctx, o, scope)
	if err != nil {
		return err
	}
	if err := layerText("id", id, maxLayerMetadataBytes, true); err != nil {
		return err
	}
	now := time.Now().UnixNano()
	result, err := s.db.ExecContext(ctx, `UPDATE layer_memories SET invalidated = 1, updated_at = ?
		WHERE id = ? AND tenant_id = ? AND scope = ? AND scope_id = ? AND invalidated = 0
		AND (expires_at IS NULL OR expires_at > ?) AND (? OR kind != 'semantic')`,
		now, id, tenant, scope, scopeID, now, o.SemanticEnabled)
	if err != nil {
		return fmt.Errorf("layer memory: forget: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("layer memory: forget count: %w", err)
	}
	if count == 0 {
		return ErrLayerNotFound
	}
	return nil
}
