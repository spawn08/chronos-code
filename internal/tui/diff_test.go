package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestWorkspaceDiffIncludesTrackedAndUntrackedEdits(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("old line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.go")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "initial")
	if err := os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("new line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.go")
	if err := os.WriteFile(filepath.Join(dir, "new file.go"), []byte("created line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := workspaceDiff(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tracked.go", "-old line", "+new line", "new file.go", "+created line", "includes existing"} {
		if !strings.Contains(got, want) {
			t.Fatalf("workspace diff missing %q: %s", want, got)
		}
	}
}

func TestWorkspaceDiffCanBeInspectedDuringTurn(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	m.sending = true
	m.input.SetValue("/diff")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || !m.sending || m.turnInterrupted {
		t.Fatal("/diff interrupted active turn or blocked Update")
	}
	m.Update(diffSnapshotMsg{id: m.diffRequestID - 1, content: "stale"})
	if m.inspection != nil {
		t.Fatal("stale diff replaced view")
	}
	m.Update(diffSnapshotMsg{id: m.diffRequestID, content: "diff --git a/file b/file\n+new line"})
	if m.inspection == nil || !strings.Contains(m.inspection.content, "+new line") || !m.sending {
		t.Fatal("workspace diff did not open as a live read-only snapshot")
	}
	m.handleInspectionKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	m.openWorkspaceDiff()
	pendingID := m.diffRequestID
	m.inspectTurn("changes")
	m.Update(diffSnapshotMsg{id: pendingID, content: "outdated diff"})
	if strings.Contains(m.inspection.content, "outdated diff") {
		t.Fatal("in-flight diff replaced a newer inspection")
	}
}
