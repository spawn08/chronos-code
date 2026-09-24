package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
)

func TestDeliveryDBCommandsBackupAndRestoreOffline(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := config.ProjectPaths{DeliveriesDB: filepath.Join(dir, "live.db")}
	backup, restored := filepath.Join(dir, "backup.db"), filepath.Join(dir, "restored.db")
	if err := runDeliveryDBCommand(ctx, paths, []string{"backup", backup}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing database was created: %v", err)
	}
	store, err := execution.OpenDeliveryStore(ctx, paths.DeliveriesDB)
	if err != nil {
		t.Fatal(err)
	}
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.Admit(ctx, execution.Admission{
		Scope: scope, DeliveryID: "delivery", AdmissionKey: "admission", Goal: execution.Goal{Statement: "persist", Actor: "operator"},
		Event: execution.EventIdentity{ID: "admit-event", IdempotencyKey: "admit-key"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runDeliveryDBCommand(ctx, paths, []string{"backup", backup}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runDeliveryDBCommand(ctx, paths, []string{"restore", backup, restored}); err != nil {
		t.Fatal(err)
	}
	result, err := execution.OpenDeliveryStore(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	if _, err := result.Load(ctx, scope, "delivery"); err != nil {
		t.Fatalf("restored admission unavailable: %v", err)
	}
	if err := runDeliveryDBCommand(ctx, paths, []string{"restore", backup, paths.DeliveriesDB}); err == nil {
		t.Fatal("restore overwrote an existing delivery database")
	}
}
