package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos/storage"
)

func TestArtifactLogRecoveryAcrossHosts(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	a := pipelineAgent(t)
	ctx := storage.WithSession(context.Background(), "s1")
	o := &Orchestrator{backgroundShells: security.NewBackgroundShells(data)}
	defer o.backgroundShells.Close()
	o.installArtifacts(a)
	shell := security.NewBackgroundShellTool(root, time.Minute, nil, o.backgroundShells)
	result, err := shell.Handler(ctx, map[string]any{"command": "printf 'FIRST\n'; i=0; while [ $i -lt 6000 ]; do printf 'fixture-output-line\n'; i=$((i+1)); done"})
	if err != nil {
		t.Fatal(err)
	}
	first := result.(map[string]any)
	if first["output_truncated"] != true {
		t.Fatal("fixture must exceed returned output")
	}
	read, _ := a.Tools.Get("read_artifact")
	out, err := read.Handler(ctx, map[string]any{"artifact_id": first["artifact_id"], "offset": 1, "limit": 10})
	if err != nil || !strings.Contains(out.(map[string]any)["content"].(string), "FIRST") {
		t.Fatalf("read=%v err=%v", out, err)
	}
	other := &Orchestrator{backgroundShells: security.NewBackgroundShells(data)}
	defer other.backgroundShells.Close()
	other.installArtifacts(a)
	read, _ = a.Tools.Get("read_artifact")
	if _, err := read.Handler(ctx, map[string]any{"artifact_id": first["artifact_id"]}); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Handler(storage.WithSession(ctx, "s2"), map[string]any{"artifact_id": first["artifact_id"]}); err == nil {
		t.Fatal("other session read artifact")
	}
	second, err := security.NewBackgroundShellTool(root, time.Minute, nil, other.backgroundShells).Handler(ctx, map[string]any{"command": "printf SECOND"})
	if err != nil {
		t.Fatal(err)
	}
	if first["log_path"] == second.(map[string]any)["log_path"] {
		t.Fatal("restart reused log path")
	}
	dataBytes, err := os.ReadFile(first["log_path"].(string))
	if err != nil || !strings.Contains(string(dataBytes), "FIRST") {
		t.Fatal("old log lost")
	}
}

func TestArtifactResultPreservesLateFinding(t *testing.T) {
	a := pipelineAgent(t)
	o := &Orchestrator{}
	o.installArtifacts(a)
	if err := saveArtifact(storage.WithSession(context.Background(), "s1"), a.Storage, "tool:report", map[string]any{"finding": strings.Repeat("context\n", 1000) + "CRITICAL_LATE_FINDING"}); err != nil {
		t.Fatal(err)
	}
	read, _ := a.Tools.Get("read_artifact")
	out, err := read.Handler(storage.WithSession(context.Background(), "s1"), map[string]any{"artifact_id": "tool:report", "offset": -3, "limit": 3})
	// JSON string fields are escaped on one line. This fixture is below the
	// per-line bound and proves the original finding remains available.
	if err != nil || !strings.Contains(out.(map[string]any)["content"].(string), "CRITICAL_LATE_FINDING") {
		t.Fatalf("result=%v error=%v", out, err)
	}
}

func TestLateBackgroundSourceMutationInvalidatesCheck(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.py")
	if err := os.WriteFile(path, []byte("value = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := newTaskRuntime("late-write", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.refreshSourceChanges(true); err != nil {
		t.Fatal(err)
	}
	code := 0
	if _, err := runtime.recordCommand("pytest", execution.CommandTest, &code, execution.TerminalExited, []execution.Scope{{Kind: execution.ScopeExact, Path: "source.py"}}, execution.ProvenanceRuntime, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range before.Verification {
		if !evidence.Current {
			t.Fatal("unchanged check was invalidated")
		}
	}
	if err := os.WriteFile(path, []byte("value = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range after.Verification {
		if evidence.Current {
			t.Fatal("late source write left check current")
		}
	}
}
