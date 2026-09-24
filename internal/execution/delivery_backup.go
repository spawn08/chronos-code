package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Integrity checks the complete SQLite database and every migration marker.
func (s *DeliveryStore) Integrity(ctx context.Context) error {
	return validateDeliveryDatabase(ctx, s.db)
}

func validateDeliveryDatabase(ctx context.Context, db *sql.DB) error {
	var status string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&status); err != nil {
		return fmt.Errorf("check delivery database integrity: %w", err)
	}
	if status != "ok" {
		return fmt.Errorf("delivery database integrity: %s: %w", status, ErrIncompatibleDeliverySchema)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check delivery foreign keys: %w", err)
	}
	if rows.Next() {
		rows.Close()
		return ErrIncompatibleDeliverySchema
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("check delivery foreign keys: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close delivery foreign key check: %w", err)
	}
	for _, migration := range deliveryMigrations {
		var checksum string
		if err := db.QueryRowContext(ctx, `SELECT checksum FROM delivery_schema_migrations WHERE version = ?`, migration.version).Scan(&checksum); err != nil || checksum != migration.checksum {
			return ErrIncompatibleDeliverySchema
		}
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM delivery_schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("check delivery schema version: %w", err)
	}
	if version > deliverySchemaVersion {
		return ErrUnsupportedDeliverySchema
	}
	if version != deliverySchemaVersion {
		return ErrIncompatibleDeliverySchema
	}
	return nil
}

// Backup creates a consistent, standalone snapshot, including uncheckpointed
// WAL changes. The destination must not exist and must differ from the store.
func (s *DeliveryStore) Backup(ctx context.Context, destination string) error {
	if err := newDeliveryDestination(s.path, destination); err != nil {
		return err
	}
	if err := s.Integrity(ctx); err != nil {
		return fmt.Errorf("backup delivery store: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, destination); err != nil {
		return fmt.Errorf("backup delivery store: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return fmt.Errorf("restrict delivery backup permissions: %w", err)
	}
	return nil
}

// RestoreDeliveryStore validates a backup and restores it to a NEW destination.
// Replacing a live database would race with workers and WAL; callers must stop
// the service and switch its configured data path after an offline restore.
func RestoreDeliveryStore(ctx context.Context, source, destination string) error {
	if source == "" {
		return ErrInvalidDelivery
	}
	if err := newDeliveryDestination(source, destination); err != nil {
		return err
	}
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("inspect delivery restore source: %w", err)
	}
	db, err := sql.Open("sqlite", source)
	if err != nil {
		return fmt.Errorf("open delivery restore source: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := validateDeliveryDatabase(ctx, db); err != nil {
		return fmt.Errorf("validate delivery restore source: %w", err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, destination); err != nil {
		return fmt.Errorf("restore delivery store: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return fmt.Errorf("restrict restored delivery permissions: %w", err)
	}
	return nil
}

func newDeliveryDestination(source, destination string) error {
	if source == "" || destination == "" || source == ":memory:" || destination == ":memory:" {
		return ErrInvalidDelivery
	}
	sourcePath, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve delivery source path: %w", err)
	}
	destinationPath, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve delivery destination path: %w", err)
	}
	if sourcePath == destinationPath {
		return ErrInvalidDelivery
	}
	if _, err := os.Lstat(destinationPath); err == nil {
		return fmt.Errorf("delivery destination already exists: %w", ErrInvalidDelivery)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect delivery destination: %w", err)
	}
	parent, err := os.Stat(filepath.Dir(destinationPath))
	if err != nil {
		return fmt.Errorf("inspect delivery destination directory: %w", err)
	}
	if !parent.IsDir() || parent.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("delivery destination %q requires a private directory (permissions %v): %w", filepath.Dir(destinationPath), parent.Mode().Perm(), ErrInvalidDelivery)
	}
	return nil
}
