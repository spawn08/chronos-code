package retention

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
)

func TestDefaultRetentionNeverDeletesAcceptedPatchesOrUndoReceipts(t *testing.T) {
	root := t.TempDir()
	accepted := []string{"patches/accepted.patch", "receipts/integration.json"}
	for _, path := range append(append([]string(nil), accepted...), "temporary.txt") {
		filename := filepath.Join(root, "artifacts", path)
		if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte("retained"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var adapter *FileAdapter
	for _, item := range DefaultAdapters(config.ProjectPaths{Dir: root}, nil) {
		if item.Scope() == ScopeInputArtifacts {
			adapter, _ = item.(*FileAdapter)
		}
	}
	if adapter == nil {
		t.Fatal("input artifact retention adapter is unavailable")
	}
	items, err := adapter.Inventory(context.Background())
	if err != nil || len(items) != 1 || items[0].Key != "temporary.txt" {
		t.Fatalf("retention inventory = %+v, error = %v", items, err)
	}
	// Even a stale queued retention item may not delete a newly accepted patch.
	toDelete := append(items, Item{Scope: ScopeInputArtifacts, Key: accepted[0]})
	if err := adapter.Delete(context.Background(), toDelete); err != nil {
		t.Fatal(err)
	}
	for _, path := range accepted {
		if _, err := os.Stat(filepath.Join(root, "artifacts", path)); err != nil {
			t.Fatalf("retention removed accepted artifact %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "artifacts", "temporary.txt")); !os.IsNotExist(err) {
		t.Fatalf("unreferenced temporary artifact was retained: %v", err)
	}
}
