package tui

import (
	"bytes"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTUILogsStayOffTerminalAndRestoreWriter(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CACHE_HOME", root)
	var terminal bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&terminal)
	t.Cleanup(func() { log.SetOutput(previous) })
	restore, err := redirectTUILogs()
	if err != nil {
		t.Fatal(err)
	}
	log.Print("graph watcher: reindex failed")
	slog.Warn("checkpoint warning")
	restore()
	if terminal.Len() != 0 {
		t.Fatalf("background logs reached terminal: %q", terminal.String())
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(cache, "chronos-code", "tui.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"graph watcher: reindex failed", "checkpoint warning"} {
		if !strings.Contains(string(contents), want) {
			t.Fatalf("log missing %q: %s", want, contents)
		}
	}
	log.Print("after TUI")
	if !strings.Contains(terminal.String(), "after TUI") {
		t.Fatal("previous log writer was not restored")
	}
}

func TestRotateTUILogKeepsBoundedPrivateGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tui.log")
	if err := os.WriteFile(path, []byte("old-current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rotateTUILog(path, 1, 2); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{path + ".1": "old-current", path + ".2": "older"} {
		got, err := os.ReadFile(name)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("current log still exists after rotation: %v", err)
	}
}

func TestRotatingLogWriterRotatesDuringSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui.log")
	w, err := openRotatingLog(path, 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("5678")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil || string(rotated) != "1234" {
		t.Fatalf("rotated log = %q, %v", rotated, err)
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != "5678" {
		t.Fatalf("current log = %q, %v", current, err)
	}
}
