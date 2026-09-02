package mcpdiscover

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatch_CreateReplaceRenameDelete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, ".mcp.json")
	updates := make(chan Snapshot, 10)
	w, err := Watch(context.Background(), root, func(snapshot Snapshot) { updates <- snapshot })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	writeFile(t, path, `{"mcpServers":{"one":{"command":"one"}}}`)
	assertUpdate(t, updates, "one", nil)

	replacement := filepath.Join(root, "replacement.json")
	writeFile(t, replacement, `{"mcpServers":{"two":{"command":"two"}}}`)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	assertUpdate(t, updates, "two", nil)

	renamed := filepath.Join(root, "renamed.json")
	if err := os.Rename(path, renamed); err != nil {
		t.Fatal(err)
	}
	assertUpdate(t, updates, "", nil)

	if err := os.Rename(renamed, path); err != nil {
		t.Fatal(err)
	}
	assertUpdate(t, updates, "two", nil)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	assertUpdate(t, updates, "", nil)
}

func TestWatch_DebouncesBursts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, ".mcp.json")
	updates := make(chan Snapshot, 10)
	w, err := Watch(context.Background(), root, func(snapshot Snapshot) { updates <- snapshot })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for i := 0; i < 5; i++ {
		writeFile(t, path, `{"mcpServers":{"latest":{"command":"latest"}}}`)
	}
	assertUpdate(t, updates, "latest", nil)
	select {
	case update := <-updates:
		t.Fatalf("burst produced an extra update: %+v", update)
	case <-time.After(2 * debounceWindow):
	}
}

func TestWatch_RetainsLastKnownGoodAndSurfacesError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, ".mcp.json")
	writeFile(t, path, `{"mcpServers":{"stable":{"command":"stable"}}}`)

	updates := make(chan Snapshot, 10)
	w, err := Watch(context.Background(), root, func(snapshot Snapshot) { updates <- snapshot })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	initial := w.Snapshot()
	if initial.Err != nil || len(initial.Servers) != 1 || initial.Servers[0].Name != "stable" {
		t.Fatalf("unexpected initial snapshot: %+v", initial)
	}

	writeFile(t, path, `{invalid`)
	invalid := receiveUpdate(t, updates)
	if invalid.Err == nil {
		t.Fatal("invalid update did not expose an error")
	}
	if len(invalid.Servers) != 1 || invalid.Servers[0].Name != "stable" {
		t.Fatalf("invalid update lost last-known-good servers: %+v", invalid)
	}
	status := w.Snapshot()
	if status.Err == nil || len(status.Servers) != 1 || status.Servers[0].Name != "stable" {
		t.Fatalf("watcher status did not retain error and servers: %+v", status)
	}

	writeFile(t, path, `{"mcpServers":{"recovered":{"command":"recovered"}}}`)
	assertUpdate(t, updates, "recovered", nil)
}

func TestWatch_CreatesMissingParentConfigDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	updates := make(chan Snapshot, 10)
	w, err := Watch(context.Background(), root, func(snapshot Snapshot) { updates <- snapshot })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	userDir := filepath.Join(home, ".chronos-code")
	if err := os.Mkdir(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(userDir, "mcp.json"), `{"mcpServers":{"user":{"command":"user"}}}`)
	assertUpdate(t, updates, "user", nil)
}

func assertUpdate(t *testing.T, updates <-chan Snapshot, wantName string, wantErr error) {
	t.Helper()
	update := receiveUpdate(t, updates)
	if (update.Err != nil) != (wantErr != nil) {
		t.Fatalf("update error=%v, want %v", update.Err, wantErr)
	}
	if wantName == "" {
		if len(update.Servers) != 0 {
			t.Fatalf("update servers=%v, want empty", update.Servers)
		}
		return
	}
	if len(update.Servers) != 1 || update.Servers[0].Name != wantName {
		t.Fatalf("update servers=%v, want only %q", update.Servers, wantName)
	}
}

func receiveUpdate(t *testing.T, updates <-chan Snapshot) Snapshot {
	t.Helper()
	select {
	case update := <-updates:
		return update
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watcher update")
		return Snapshot{}
	}
}
