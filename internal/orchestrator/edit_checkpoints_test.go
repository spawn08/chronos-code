package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/config"
)

func checkpointOrch(t *testing.T) *Orchestrator {
	t.Helper()
	t.Setenv("CHRONOS_CODE_DATA_HOME", t.TempDir())
	cfg := &config.Config{}
	cfg.Workspace.Root = t.TempDir()
	return &Orchestrator{cfg: cfg, active: "editor", sessions: map[string]string{"editor": "session-one"}}
}

func checkpointWrite(t *testing.T, o *Orchestrator, path, body string) {
	t.Helper()
	input := map[string]any{"path": path}
	h := sessionUXHook{orchestrator: o}
	if err := h.Before(context.Background(), &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_write", Input: input}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.resolvePath(path), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.After(context.Background(), &hooks.Event{Type: hooks.EventToolCallAfter, Name: "file_write", Input: input}); err != nil {
		t.Fatal(err)
	}
}

func assertEditBody(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("read %s = %q, %v; want %q", path, got, err, want)
	}
}

func TestEditCheckpointRestartAndPermissions(t *testing.T) {
	o := checkpointOrch(t)
	path := o.resolvePath("script")
	if err := os.WriteFile(path, []byte("original"), 0o751); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"path": "script"}
	o.snapshotWrite(input)
	if err := os.WriteFile(path, []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	o.commitWrite(input)
	cp := o.edits[len(o.edits)-1]
	if len(cp.Prev) != 0 || cp.PostMode != 0o640 || cp.PrevMode != 0o751 {
		t.Fatalf("unexpected checkpoint metadata: %+v", cp)
	}
	for _, artifact := range []string{cp.Journal, filepath.Join(filepath.Dir(cp.Journal), cp.Snapshot)} {
		info, err := os.Stat(artifact)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact permissions: %v, %v", info, err)
		}
	}
	if info, err := os.Stat(filepath.Dir(cp.Journal)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory permissions: %v, %v", info, err)
	}
	restarted := &Orchestrator{cfg: o.cfg, active: "editor", sessions: map[string]string{"editor": "different-session"}}
	if _, err := restarted.UndoLastEdit(); err == nil || !strings.Contains(err.Error(), "nothing to undo") {
		t.Fatalf("another session recovered edits: %v", err)
	}
	restarted.sessions["editor"] = "session-one"
	if got, err := restarted.UndoLastEdit(); err != nil || got != "script" {
		t.Fatalf("restart undo = %q, %v", got, err)
	}
	assertEditBody(t, path, "original")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o751 {
		t.Fatalf("restored mode: %v, %v", info, err)
	}
	if _, err := restarted.UndoLastEdit(); err == nil {
		t.Fatal("undone journal was recovered again")
	}
}

func TestEditCheckpointNewFileAndFallback(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk=%v", disk), func(t *testing.T) {
			o := &Orchestrator{}
			path := filepath.Join(t.TempDir(), "new.txt")
			if disk {
				o = checkpointOrch(t)
				path = "new.txt"
			}
			checkpointWrite(t, o, path, "first")
			checkpointWrite(t, o, path, "second")
			if disk {
				o = &Orchestrator{cfg: o.cfg, active: o.active, sessions: o.sessions}
			}
			if _, err := o.UndoLastEdit(); err != nil {
				t.Fatal(err)
			}
			assertEditBody(t, o.resolvePath(path), "first")
			if _, err := o.UndoLastEdit(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(o.resolvePath(path)); !os.IsNotExist(err) {
				t.Fatalf("new file remains: %v", err)
			}
			if disk {
				o.edits = nil
				if _, err := o.UndoLastEdit(); err == nil {
					t.Fatal("deletion was not persisted")
				}
			}
		})
	}
}

func TestEditCheckpointConflictsRetainSnapshot(t *testing.T) {
	for _, conflict := range []string{"content", "mode", "missing", "symlink"} {
		t.Run(conflict, func(t *testing.T) {
			o := checkpointOrch(t)
			checkpointWrite(t, o, "note", "agent")
			path := o.resolvePath("note")
			switch conflict {
			case "content":
				if err := os.WriteFile(path, []byte("user"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing", "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if conflict == "symlink" {
					target := o.resolvePath("target")
					if err := os.WriteFile(target, []byte("agent"), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := o.UndoLastEdit(); err == nil || !strings.Contains(err.Error(), "conflict") {
				t.Fatalf("expected conflict: %v", err)
			}
			if len(o.edits) != 1 || o.edits[0].State != "committed" {
				t.Fatal("conflict discarded checkpoint")
			}
			if conflict == "content" {
				assertEditBody(t, path, "user")
			}
			// Restore the expected post-write state to demonstrate retryability.
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("agent"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := o.UndoLastEdit(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEditCheckpointFailedWriteAndPendingRecovery(t *testing.T) {
	o := checkpointOrch(t)
	checkpointWrite(t, o, "note", "first")
	input := map[string]any{"path": "note"}
	o.snapshotWrite(input)
	cp := o.edits[len(o.edits)-1]
	restarted := &Orchestrator{cfg: o.cfg, active: o.active, sessions: o.sessions}
	// A pending snapshot must never be mistaken for a confirmed edit.
	if err := restarted.recoverEdits("session-one"); err != nil || len(restarted.edits) != 1 {
		t.Fatalf("pending recovery: %d, %v", len(restarted.edits), err)
	}
	errTool := errors.New("original write failure")
	evt := &hooks.Event{Type: hooks.EventToolCallAfter, Name: "file_write", Input: input, Error: errTool}
	if err := (sessionUXHook{orchestrator: o}).After(context.Background(), evt); err != nil || evt.Error != errTool {
		t.Fatalf("masked tool failure: %v, %v", err, evt.Error)
	}
	if len(o.edits) != 1 {
		t.Fatalf("failed checkpoint retained: %d", len(o.edits))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cp.Journal), cp.Snapshot)); !os.IsNotExist(err) {
		t.Fatalf("failed snapshot remains: %v", err)
	}
	o.edits = nil
	if _, err := o.UndoLastEdit(); err != nil {
		t.Fatal(err)
	}
}

func TestEditCheckpointLargeFileBounds(t *testing.T) {
	o := checkpointOrch(t)
	path := o.resolvePath("large")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(32 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := o.prepareWrite(map[string]any{"path": "large"}, "session-one"); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("32 MiB snapshot allocated %d bytes", allocated)
	}
	if len(o.edits[0].Prev) != 0 {
		t.Fatal("disk checkpoint retained file body")
	}
	fallback := &Orchestrator{}
	if err := fallback.prepareWrite(map[string]any{"path": path}, ""); err == nil {
		t.Fatal("large fallback snapshot accepted")
	}
	o.discardWrite(map[string]any{"path": "large"}, "session-one")
	if err := os.Truncate(path, maxEditSnapshotBytes+1); err != nil {
		t.Fatal(err)
	}
	h := sessionUXHook{orchestrator: o}
	if err := h.Before(context.Background(), &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_write", Input: map[string]any{"path": "large"}}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized hook did not block write: %v", err)
	}
}

func TestEditCheckpointRejectsUnreadableOriginal(t *testing.T) {
	o := checkpointOrch(t)
	path := o.resolvePath("directory")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := o.prepareWrite(map[string]any{"path": "directory"}, "session-one"); err == nil {
		t.Fatal("directory treated as missing file")
	}
	path = o.resolvePath("private")
	if err := os.WriteFile(path, []byte("private"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if os.Geteuid() != 0 {
		if err := o.prepareWrite(map[string]any{"path": "private"}, "session-one"); err == nil {
			t.Fatal("unreadable file treated as missing")
		}
	}
	if len(o.edits) != 0 {
		t.Fatal("read error created checkpoint")
	}
}

func TestEditCheckpointMetadataBoundAndContextSession(t *testing.T) {
	o := checkpointOrch(t)
	for i := 0; i < maxEditCheckpoints+2; i++ {
		checkpointWrite(t, o, "note", fmt.Sprint(i))
	}
	if len(o.edits) != maxEditCheckpoints {
		t.Fatalf("metadata count = %d", len(o.edits))
	}
	dir, err := o.editCheckpointDir("session-one")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(entries) != maxEditCheckpoints+2 {
		t.Fatalf("journals pruned: %d, %v", len(entries), err)
	}
	o.edits = nil
	for i := 0; i < maxEditCheckpoints+2; i++ {
		if _, err := o.UndoLastEdit(); err != nil {
			t.Fatalf("undo %d: %v", i, err)
		}
		if len(o.edits) > maxEditCheckpoints {
			t.Fatal("recovery exceeded metadata bound")
		}
	}
	ctx := storage.WithSession(context.Background(), "context-session")
	h := sessionUXHook{orchestrator: o}
	input := map[string]any{"path": "context-note"}
	if err := h.Before(ctx, &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_write", Input: input}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.resolvePath("context-note"), []byte("context"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.After(ctx, &hooks.Event{Type: hooks.EventToolCallAfter, Name: "file_write", Input: input}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.UndoLastEdit(); err == nil {
		t.Fatal("active session undid context session's edit")
	}
	o.sessions["editor"] = "context-session"
	if _, err := o.UndoLastEdit(); err != nil {
		t.Fatal(err)
	}
}

func TestEditCheckpointFallbackAggregateBudget(t *testing.T) {
	o := &Orchestrator{}
	root := t.TempDir()
	for i := 0; i < 2; i++ {
		path := filepath.Join(root, fmt.Sprint(i))
		if err := os.WriteFile(path, make([]byte, maxEditMemoryBytes/2), 0o644); err != nil {
			t.Fatal(err)
		}
		checkpointWrite(t, o, path, "edited")
	}
	third := filepath.Join(root, "third")
	if err := os.WriteFile(third, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := o.prepareWrite(map[string]any{"path": third}, ""); err == nil {
		t.Fatal("aggregate fallback budget exceeded")
	}
	assertEditBody(t, third, "original")
	if _, err := o.UndoLastEdit(); err != nil {
		t.Fatal(err)
	}
	checkpointWrite(t, o, third, "edited")
}

func TestEditCheckpointPartialFailurePreservesOriginalError(t *testing.T) {
	for _, cleanupFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanup-failure=%v", cleanupFailure), func(t *testing.T) {
			o := checkpointOrch(t)
			checkpointWrite(t, o, "note", "agent")
			input := map[string]any{"path": "note"}
			o.snapshotWrite(input)
			if err := os.WriteFile(o.resolvePath("note"), []byte("partial"), 0o644); err != nil {
				t.Fatal(err)
			}
			if cleanupFailure {
				cp := o.edits[len(o.edits)-1]
				if err := os.Remove(cp.Journal); err != nil {
					t.Fatal(err)
				}
				// Force the atomic rename to fail, even when tests run as root.
				if err := os.Mkdir(cp.Journal, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			failure := errors.New("disk full during write")
			evt := &hooks.Event{Type: hooks.EventToolCallAfter, Name: "file_write", Input: input, Error: failure}
			if err := (sessionUXHook{orchestrator: o}).After(context.Background(), evt); err != nil || evt.Error != failure {
				t.Fatalf("original error masked: %v, %v", err, evt.Error)
			}
			if len(o.edits) != 1 {
				t.Fatal("failed write remained in undo history")
			}
			if _, err := o.UndoLastEdit(); err == nil {
				t.Fatal("undo overwrote partial write")
			}
			assertEditBody(t, o.resolvePath("note"), "partial")
		})
	}
}

func TestEditCheckpointCorruptSnapshotRetainsCheckpoint(t *testing.T) {
	o := checkpointOrch(t)
	path := o.resolvePath("note")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkpointWrite(t, o, "note", "agent")
	cp := o.edits[0]
	if err := os.WriteFile(filepath.Join(filepath.Dir(cp.Journal), cp.Snapshot), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := o.UndoLastEdit(); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("corrupt snapshot accepted: %v", err)
	}
	assertEditBody(t, path, "agent")
	if len(o.edits) != 1 {
		t.Fatal("restore error lost checkpoint")
	}
}
