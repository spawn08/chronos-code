package security

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/tool/builtins"
)

func TestWorkspaceShellUsesRequestedContainedDirectory(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := NewWorkspaceShellTool(root, time.Second).Handler(context.Background(), map[string]any{"command": "pwd", "working_dir": "subdir"})
	if err != nil {
		t.Fatal(err)
	}
	values := result.(map[string]any)
	want := subdir
	if canonical, canonicalErr := filepath.EvalSymlinks(want); canonicalErr == nil {
		want = canonical
	}
	if filepath.Clean(strings.TrimSpace(values["stdout"].(string))) != want {
		t.Fatalf("pwd = %q, want %q", values["stdout"], want)
	}
}

func TestWorkspaceShellRejectsOutsideDirectory(t *testing.T) {
	root := t.TempDir()
	_, err := NewWorkspaceShellTool(root, time.Second).Handler(context.Background(), map[string]any{"command": "pwd", "working_dir": filepath.Dir(root)})
	if err == nil {
		t.Fatal("outside working directory was accepted")
	}
}

func TestWorkspaceShellTimeoutReturnsContextError(t *testing.T) {
	_, err := NewWorkspaceShellTool(t.TempDir(), 10*time.Millisecond).Handler(context.Background(), map[string]any{"command": "sleep 10"})
	if err == nil {
		t.Fatal("timed out shell returned nil error")
	}
}

func TestWorkspaceShellUsesRequestScopedRoot(t *testing.T) {
	configured := t.TempDir()
	requestRoot := t.TempDir()
	result, err := NewWorkspaceShellTool(configured, time.Second).Handler(
		builtins.WithWorkspaceRoot(context.Background(), requestRoot),
		map[string]any{"command": "pwd"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := requestRoot
	if canonical, canonicalErr := filepath.EvalSymlinks(want); canonicalErr == nil {
		want = canonical
	}
	if got := filepath.Clean(strings.TrimSpace(result.(map[string]any)["stdout"].(string))); got != want {
		t.Fatalf("pwd = %q, want request root %q", got, want)
	}
}

func TestMandatorySandboxRefusesIncompleteReadIsolation(t *testing.T) {
	root := t.TempDir()
	ctx := WithMandatorySandbox(context.Background(), SandboxPolicy{})
	_, err := NewWorkspaceShellTool(root, 5*time.Second).Handler(ctx, map[string]any{"command": "touch should-not-exist"})
	if !errors.Is(err, ErrMandatorySandboxUnavailable) {
		t.Fatalf("mandatory execution error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("mandatory execution ran an effect: %v", err)
	}
}
