package learning

import (
	"context"
	"database/sql"
	"fmt"
)

const schemaVersion = 1

const schemaV1 = `
CREATE TABLE IF NOT EXISTS sessions (
	id            TEXT PRIMARY KEY,
	repo_path     TEXT NOT NULL,
	started_at    TEXT NOT NULL,
	ended_at      TEXT,
	model         TEXT NOT NULL DEFAULT '',
	turns         INTEGER NOT NULL DEFAULT 0,
	input_tokens  INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd      REAL NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS turns (
	id         TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	role       TEXT NOT NULL,
	content    TEXT NOT NULL DEFAULT '',
	ts         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_turns_session_ts ON turns(session_id, ts, id);

CREATE TABLE IF NOT EXISTS tool_calls (
	id          TEXT PRIMARY KEY,
	turn_id     TEXT NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	input       TEXT NOT NULL DEFAULT '',
	output      TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	ts          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tool_calls_turn_ts ON tool_calls(turn_id, ts, id);

CREATE TABLE IF NOT EXISTS outcomes (
	turn_id   TEXT PRIMARY KEY REFERENCES turns(id) ON DELETE CASCADE,
	kind      TEXT NOT NULL CHECK (kind IN ('accepted', 'rejected', 'edited', 'reverted')),
	user_edit TEXT NOT NULL DEFAULT '',
	ts        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS patterns (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_path        TEXT NOT NULL,
	trigger_hash     TEXT NOT NULL,
	solution_summary TEXT NOT NULL DEFAULT '',
	tool_sequence    TEXT NOT NULL DEFAULT '',
	success_count    INTEGER NOT NULL DEFAULT 0,
	fail_count       INTEGER NOT NULL DEFAULT 0,
	last_used_at     TEXT,
	embedding        BLOB,
	UNIQUE(repo_path, trigger_hash)
);
CREATE INDEX IF NOT EXISTS idx_patterns_repo_success
	ON patterns(repo_path, success_count DESC, fail_count, id);
`

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("learning: read schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("learning: database schema version %d is newer than supported version %d", version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("learning: begin schema migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
		return fmt.Errorf("learning: migrate schema to version 1: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		return fmt.Errorf("learning: set schema version 1: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("learning: commit schema migration: %w", err)
	}
	return nil
}
