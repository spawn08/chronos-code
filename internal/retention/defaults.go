package retention

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/worktree"
)

const (
	ScopeSessions        = "sessions"
	ScopeEvents          = "events"
	ScopeTraces          = "traces"
	ScopeAudit           = "audit"
	ScopeCheckpoints     = "checkpoints"
	ScopeSessionFiles    = "session_files"
	ScopeTelemetry       = "telemetry"
	ScopeLayeredMemory   = "layered_memory"
	ScopeEditCheckpoints = "edit_checkpoints"
	ScopeInputArtifacts  = "input_artifacts"
	ScopeWorktrees       = "worktrees"
	ScopeTUILogs         = "tui_logs"
	ScopePlanDB          = "plan_db"
	ScopeLimiterEntries  = "limiter_entries"
)

// DefaultAdapters returns every locally accessible retention surface. The plan
// database is deliberately handled only by explicit scoped prune because its
// schema has no timestamps and deleting the database would destroy active work.
func DefaultAdapters(paths config.ProjectPaths, activeSessionIDs []string) []Adapter {
	active := make(map[string]bool, len(activeSessionIDs))
	var activeCheckpointDirs []string
	for _, id := range activeSessionIDs {
		active[id] = true
		activeCheckpointDirs = append(activeCheckpointDirs, fmt.Sprintf("%x", sha256.Sum256([]byte(id))))
	}
	session := func(name, table, timestamp, sessionColumn, size string) Adapter {
		return &SQLiteResource{Name: name, Path: paths.SessionsDB, Table: table, TimeColumn: timestamp, SessionColumn: sessionColumn, SizeExpr: size, Active: active, CascadeSession: table == "sessions"}
	}
	var adapters []Adapter
	if localSQLitePath(paths.SessionsDB) {
		adapters = append(adapters,
			session(ScopeEvents, "events", "created_at", "session_id", byteExpr("payload")),
			session(ScopeTraces, "traces", "started_at", "session_id", byteExpr("input", "output", "error")),
			session(ScopeAudit, "audit_logs", "created_at", "session_id", byteExpr("detail")),
			session(ScopeCheckpoints, "checkpoints", "created_at", "session_id", byteExpr("state")),
			session(ScopeSessionFiles, "session_files", "updated_at", "session_id", "size"),
			session(ScopeSessions, "sessions", "updated_at", "id", byteExpr("metadata")),
		)
	}
	adapters = append(adapters,
		&SQLiteResource{Name: ScopeTelemetry, Path: paths.TelemetryDB, Table: "sessions", TimeColumn: "started_at", SessionColumn: "id", SizeExpr: byteExpr("model", "repo_path"), Active: active},
		&SQLiteResource{Name: ScopeLayeredMemory, Path: paths.MemoryDB, Table: "layer_memories", TimeColumn: "updated_at", SessionColumn: "session_id", SizeExpr: byteExpr("content", "steps"), Active: active},
		&FileAdapter{Name: ScopeEditCheckpoints, Root: filepath.Join(paths.Dir, "edit-checkpoints"), ActivePrefixes: activeCheckpointDirs},
		&FileAdapter{Name: ScopeInputArtifacts, Root: filepath.Join(paths.Dir, "artifacts"), ActivePrefixes: []string{"patches", "receipts"}},
		ReadOnlyFileAdapter{Name: ScopePlanDB, Path: paths.PlansDB},
	)
	if manager, err := worktree.New(paths.Dir, nil); err == nil {
		adapters = append(adapters, &WorktreeAdapter{Manager: manager})
	}
	if cache, err := os.UserCacheDir(); err == nil {
		adapters = append(adapters, &FileAdapter{Name: ScopeTUILogs, Root: filepath.Join(cache, "chronos-code")})
	}
	return adapters
}

func localSQLitePath(path string) bool {
	if path == "" || path == ":memory:" {
		return false
	}
	return !strings.Contains(path, "://") && !strings.HasPrefix(path, "file:")
}
