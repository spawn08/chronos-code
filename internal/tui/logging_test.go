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
