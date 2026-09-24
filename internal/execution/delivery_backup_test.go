package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeliveryBackupRestorePreservesQueuedEventsAndRejectsUnsafeTargets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path, backup, restored := filepath.Join(dir, "live.db"), filepath.Join(dir, "backup.db"), filepath.Join(dir, "restored.db")
	store := openTestDeliveryStore(t, path)
	admission := testAdmission("tenant", "repo", "delivery", "key")
	if _, err := store.AdmitRunnable(ctx, admission); err != nil {
		t.Fatal(err)
	}
	if err := store.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup permissions = %v, error = %v", info, err)
	}
	if err := store.Backup(ctx, backup); !errors.Is(err, ErrInvalidDelivery) {
		t.Fatalf("overwrite backup = %v", err)
	}
	if err := store.Backup(ctx, path); !errors.Is(err, ErrInvalidDelivery) {
		t.Fatalf("backup onto source = %v", err)
	}
	lease, err := store.Claim(ctx, "original", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finalize(ctx, lease, Outcome{Kind: OutcomeFailed}); err != nil {
		t.Fatal(err)
	}
	if err := RestoreDeliveryStore(ctx, backup, restored); err != nil {
		t.Fatal(err)
	}
	restoredStore := openTestDeliveryStore(t, restored)
	if err := restoredStore.Integrity(ctx); err != nil {
		t.Fatal(err)
	}
	delivery, err := restoredStore.Load(ctx, admission.Scope, admission.DeliveryID)
	if err != nil || delivery.State != DeliveryQueued {
		t.Fatalf("restored delivery = %#v, error = %v", delivery, err)
	}
	events, err := restoredStore.Events(ctx, admission.Scope, admission.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayDelivery(events)
	if err != nil || replayed.State != DeliveryQueued {
		t.Fatalf("restored event replay = %#v, error = %v", replayed, err)
	}
	if _, err := restoredStore.Claim(ctx, "replacement", time.Minute); err != nil {
		t.Fatalf("restored queue not runnable: %v", err)
	}
	if err := RestoreDeliveryStore(ctx, backup, restored); !errors.Is(err, ErrInvalidDelivery) {
		t.Fatalf("overwrite restored database = %v", err)
	}
	original, err := store.Load(ctx, admission.Scope, admission.DeliveryID)
	if err != nil || original.State != DeliveryFailed {
		t.Fatalf("original database modified by restore = %#v, error = %v", original, err)
	}
}

func TestDeliveryBackupRejectsSharedDestinationDirectory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store := openTestDeliveryStore(t, filepath.Join(root, "live.db"))
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(shared, "backup.db")
	if err := store.Backup(ctx, path); !errors.Is(err, ErrInvalidDelivery) {
		t.Fatalf("shared-directory backup = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("backup created in shared directory: %v", err)
	}
}

func TestDeliveryRestoreRejectsInvalidAndFutureSourcesWithoutCreatingDestination(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	source, destination := filepath.Join(dir, "source.db"), filepath.Join(dir, "destination.db")
	store := openTestDeliveryStore(t, source)
	if _, err := store.db.ExecContext(ctx, `UPDATE delivery_schema_migrations SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := RestoreDeliveryStore(ctx, source, destination); !errors.Is(err, ErrIncompatibleDeliverySchema) {
		t.Fatalf("tampered source = %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("invalid restore created destination: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE delivery_schema_migrations SET checksum = ? WHERE version = 1`, deliveryMigrations[0].checksum); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO delivery_schema_migrations (version, checksum) VALUES (?, 'future')`, deliverySchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := RestoreDeliveryStore(ctx, source, destination); !errors.Is(err, ErrUnsupportedDeliverySchema) {
		t.Fatalf("future source = %v", err)
	}
}
