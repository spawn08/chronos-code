package retention

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteResource struct {
	Name           string
	Path           string
	Table          string
	TimeColumn     string
	SessionColumn  string
	SizeExpr       string
	Active         map[string]bool
	CascadeSession bool
}

func (a *SQLiteResource) Scope() string { return a.Name }

func (a *SQLiteResource) Inventory(ctx context.Context) ([]Item, error) {
	if _, err := os.Stat(a.Path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if !validSQLName(a.Table) || !validSQLName(a.TimeColumn) || (a.SessionColumn != "" && !validSQLName(a.SessionColumn)) {
		return nil, fmt.Errorf("invalid SQLite retention schema")
	}
	db, err := sql.Open("sqlite", a.Path+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, a.Table).Scan(&exists); err != nil || exists == 0 {
		return nil, err
	}
	sizeExpr := a.SizeExpr
	if sizeExpr == "" {
		sizeExpr = "0"
	}
	sessionExpr := "''"
	if a.SessionColumn != "" {
		sessionExpr = a.SessionColumn
	}
	query := fmt.Sprintf(`SELECT rowid, %s, %s, COALESCE(%s,0) FROM %s ORDER BY %s DESC, rowid`, sessionExpr, a.TimeColumn, sizeExpr, a.Table, a.TimeColumn)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var rowID int64
		var sessionID string
		var raw any
		var size int64
		if err := rows.Scan(&rowID, &sessionID, &raw, &size); err != nil {
			return nil, err
		}
		if a.Active[sessionID] {
			continue
		}
		at, err := sqliteTime(raw)
		if err != nil {
			return nil, fmt.Errorf("parse %s timestamp: %w", a.Name, err)
		}
		items = append(items, Item{Scope: a.Name, Key: strconv.FormatInt(rowID, 10), UpdatedAt: at, Bytes: size})
	}
	return items, rows.Err()
}

func (a *SQLiteResource) Delete(ctx context.Context, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	db, err := sql.Open("sqlite", a.Path+"?mode=rw&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range items {
		rowID, err := strconv.ParseInt(item.Key, 10, 64)
		if err != nil {
			return err
		}
		if a.CascadeSession {
			var tenant, sessionID string
			if err := tx.QueryRowContext(ctx, `SELECT tenant_id,id FROM sessions WHERE rowid=?`, rowID).Scan(&tenant, &sessionID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return err
			}
			for _, table := range []string{"events", "checkpoints", "traces", "audit_logs", "memory", "session_files"} {
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE tenant_id=? AND session_id=?`, tenant, sessionID); err != nil {
					return err
				}
			}
		}
		if a.Table == "layer_memories" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM layer_memories_fts WHERE rowid=?`, rowID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+a.Table+` WHERE rowid=?`, rowID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func validSQLName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func sqliteTime(value any) (time.Time, error) {
	switch value := value.(type) {
	case time.Time:
		return value.UTC(), nil
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed.UTC(), nil
			}
		}
	case []byte:
		return sqliteTime(string(value))
	case int64:
		return time.Unix(0, value).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unsupported value %T", value)
}

func byteExpr(columns ...string) string {
	parts := make([]string, 0, len(columns))
	for _, column := range columns {
		parts = append(parts, "length(COALESCE("+column+",''))")
	}
	return strings.Join(parts, "+")
}
