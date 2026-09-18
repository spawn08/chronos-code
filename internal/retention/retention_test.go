package retention

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type testAdapter struct {
	items   []Item
	deleted []Item
}

func (a *testAdapter) Scope() string { return "test" }
func (a *testAdapter) Inventory(context.Context) ([]Item, error) {
	return append([]Item(nil), a.items...), nil
}
func (a *testAdapter) Delete(_ context.Context, items []Item) error {
	a.deleted = append(a.deleted, items...)
	return nil
}

func TestSoakBoundedBatchesAndDryRunParity(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	adapter := &testAdapter{}
	for i := 0; i < 10000; i++ {
		adapter.items = append(adapter.items, Item{Scope: "test", Key: string(rune(i + 1)), UpdatedAt: now.Add(-time.Duration(i) * time.Minute), Bytes: 10})
	}
	m := Manager{Adapters: []Adapter{adapter}, Policies: map[string]Policy{"test": {MaxCount: 10}}, BatchSize: 37, Now: func() time.Time { return now }}
	dry, err := m.Run(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	real, err := m.Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Candidates) != 37 || len(real.Candidates) != 37 || !reflect.DeepEqual(dry.Candidates, real.Candidates) || dry.ReclaimedBytes != real.ReclaimedBytes || len(adapter.deleted) != 37 {
		t.Fatalf("dry=%+v real=%+v deleted=%d", dry, real, len(adapter.deleted))
	}
}

func TestInterruptedRenameRecoveryAndSymlinkContainment(t *testing.T) {
	root := t.TempDir()
	trash := filepath.Join(root, trashDirName)
	if err := os.Mkdir(trash, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trash, "interrupted"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := Recover(root, 10); err != nil {
		t.Fatal(err)
	}
	if err := Recover(root, 10); err != nil {
		t.Fatalf("second recovery was not idempotent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(trash, "interrupted")); !os.IsNotExist(err) {
		t.Fatalf("tombstone still exists: %v", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatalf("outside file changed: %q, %v", data, err)
	}
}

func TestPolicyLimitsAreIndependentAndZeroDisablesOnlyOneLimit(t *testing.T) {
	now := time.Now().UTC()
	items := []Item{
		{Key: "new", UpdatedAt: now, Bytes: 8},
		{Key: "middle", UpdatedAt: now.Add(-time.Hour), Bytes: 8},
		{Key: "old", UpdatedAt: now.Add(-48 * time.Hour), Bytes: 8},
	}
	selected := selectCandidates(items, Policy{MaxAge: 24 * time.Hour, MaxCount: 0, MaxBytes: 16}, now)
	keys := make(map[string]bool)
	for _, item := range selected {
		keys[item.Key] = true
	}
	if len(keys) != 1 || !keys["old"] {
		t.Fatalf("candidates = %+v", selected)
	}
	if selected := selectCandidates(items, Policy{}, now); len(selected) != 0 {
		t.Fatalf("zero policy candidates = %+v", selected)
	}
}

func TestActiveResourceExcluded(t *testing.T) {
	// Active exclusion is applied by adapters before policy selection.
	a := &FileAdapter{Name: "test", Root: t.TempDir(), ActiveKeys: map[string]bool{"active": true}}
	if err := os.WriteFile(filepath.Join(a.Root, "active"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Root, "idle"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := a.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Key != "idle" {
		t.Fatalf("inventory = %+v", items)
	}
}
