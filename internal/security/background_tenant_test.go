package security

import (
	"context"
	"testing"
	"time"

	"github.com/spawn08/chronos/storage"
)

func TestBackgroundShellTenantIsolation(t *testing.T) {
	shells := NewBackgroundShells(t.TempDir())
	defer shells.Close()
	ctx := storage.WithTenant(storage.WithSession(context.Background(), "same-session"), "owner")
	out, err := NewBackgroundShellTool(t.TempDir(), time.Minute, nil, shells).Handler(ctx, map[string]any{"command": "sleep 30", "block_until_ms": 0})
	if err != nil {
		t.Fatal(err)
	}
	id := out.(map[string]any)["shell_id"].(string)
	other := storage.WithTenant(ctx, "other")
	if _, err := shells.await(other, id, nil, 0); err == nil {
		t.Fatal("another tenant awaited shell")
	}
	if _, err := shells.kill(other, id); err == nil {
		t.Fatal("another tenant cancelled shell")
	}
	if lines := shells.RunningContext(other); len(lines) != 0 {
		t.Fatalf("another tenant saw shell status: %v", lines)
	}
	if lines := shells.RunningContext(ctx); len(lines) != 1 {
		t.Fatalf("owner lost shell status: %v", lines)
	}
	if _, err := shells.ReadLog(other, id); err == nil {
		t.Fatal("another tenant read shell log")
	}
	if _, err := shells.kill(ctx, id); err != nil {
		t.Fatal(err)
	}
}
