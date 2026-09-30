package security

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
)

type recordedAsk struct {
	dir    string
	target string
	access DirectoryAccess
}

func outsideFixture(t *testing.T) (workspace, outside, file string) {
	t.Helper()
	workspace = t.TempDir()
	outside = t.TempDir()
	file = filepath.Join(outside, "notes.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	return workspace, canonical, file
}

func toolEvent(name string, args map[string]any) *hooks.Event {
	return &hooks.Event{Type: hooks.EventToolCallBefore, Name: name, Input: args}
}

func TestGuardOutsideReadDeniedWithoutDirectoryApproval(t *testing.T) {
	workspace, _, file := outsideFixture(t)
	policy := &Policy{ReadablePaths: []string{"."}, WritablePaths: []string{"."}}
	guard := NewGuard(policy, workspace, nil)
	err := guard.Before(context.Background(), toolEvent("file_read", map[string]any{"path": file}))
	if err == nil || !strings.Contains(err.Error(), "outside all readable_paths") {
		t.Fatalf("expected readable_paths denial, got %v", err)
	}
}

func TestGuardAsksOnceThenRemembersDirectoryForSession(t *testing.T) {
	workspace, outside, file := outsideFixture(t)
	policy := &Policy{ReadablePaths: []string{"."}, WritablePaths: []string{"."}}
	var asks []recordedAsk
	policy.SetDirectoryApproval(func(_ context.Context, dir, target string, access DirectoryAccess) (bool, error) {
		asks = append(asks, recordedAsk{dir, target, access})
		return true, nil
	})
	guard := NewGuard(policy, workspace, nil)
	for i := 0; i < 2; i++ {
		if err := guard.Before(context.Background(), toolEvent("file_read", map[string]any{"path": file})); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if len(asks) != 1 || asks[0].dir != outside || asks[0].access != DirectoryRead {
		t.Fatalf("asks = %+v, want one read ask for %s", asks, outside)
	}

	// A read grant does not cover writes: the user is asked again.
	if err := guard.Before(context.Background(), toolEvent("file_write", map[string]any{"path": file, "content": "x"})); err != nil {
		t.Fatalf("write after approval: %v", err)
	}
	if len(asks) != 2 || asks[1].access != DirectoryWrite {
		t.Fatalf("asks = %+v, want a second write ask", asks)
	}
	if got := policy.SessionDirectories(); len(got) != 1 || got[0] != outside+" (read/write)" {
		t.Fatalf("SessionDirectories = %v", got)
	}
}

func TestGuardDeniedDirectoryApprovalBlocks(t *testing.T) {
	workspace, _, file := outsideFixture(t)
	policy := &Policy{ReadablePaths: []string{"."}}
	policy.SetDirectoryApproval(func(context.Context, string, string, DirectoryAccess) (bool, error) { return false, nil })
	guard := NewGuard(policy, workspace, nil)
	if err := guard.Before(context.Background(), toolEvent("file_list", map[string]any{"path": filepath.Dir(file)})); err == nil {
		t.Fatal("declined directory must stay blocked")
	}
	if len(policy.SessionDirectories()) != 0 {
		t.Fatal("declined directory must not be granted")
	}
}

func TestGuardDeniedPathNeverPrompts(t *testing.T) {
	workspace, _, file := outsideFixture(t)
	policy := &Policy{ReadablePaths: []string{"."}, DeniedPaths: []string{"**/notes.txt"}}
	asked := false
	policy.SetDirectoryApproval(func(context.Context, string, string, DirectoryAccess) (bool, error) { asked = true; return true, nil })
	guard := NewGuard(policy, workspace, nil)
	if err := guard.Before(context.Background(), toolEvent("file_read", map[string]any{"path": file})); err == nil {
		t.Fatal("denied path must be blocked")
	}
	if asked {
		t.Fatal("denied path must not prompt")
	}
}

func TestShellWorkingDirOutsideWorkspaceAsksAndRuns(t *testing.T) {
	workspace, outside, _ := outsideFixture(t)
	policy := &Policy{ReadablePaths: []string{"."}, WritablePaths: []string{"."}}
	guard := NewGuard(policy, workspace, nil)
	args := map[string]any{"command": "pwd", "working_dir": outside}

	if err := guard.Before(context.Background(), toolEvent("shell", args)); err == nil {
		t.Fatal("outside working_dir must be blocked without approval")
	}
	policy.SetDirectoryApproval(func(_ context.Context, dir string, _ string, access DirectoryAccess) (bool, error) {
		return dir == outside && access == DirectoryWrite, nil
	})
	if err := guard.Before(context.Background(), toolEvent("shell", args)); err != nil {
		t.Fatalf("approved working_dir blocked: %v", err)
	}
	result, err := NewWorkspaceShellToolWithPolicy(workspace, 5*time.Second, policy).Handler(context.Background(), args)
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	if got := strings.TrimSpace(result.(map[string]any)["stdout"].(string)); got != outside {
		t.Fatalf("pwd = %q, want %q", got, outside)
	}
}

func TestPermissionCheckerDefersOutsidePathOnlyWithDirectoryApproval(t *testing.T) {
	workspace, _, file := outsideFixture(t)
	policy := &Policy{ReadablePaths: []string{"."}}
	checker := NewPermissionChecker(policy, workspace)
	args := map[string]any{"path": file}
	if got := checker.Check("file_read", args, false); got != Deny {
		t.Fatalf("without handler = %s, want deny", got)
	}
	policy.SetDirectoryApproval(func(context.Context, string, string, DirectoryAccess) (bool, error) { return true, nil })
	if got := checker.Check("file_read", args, false); got == Deny {
		t.Fatal("with handler the guard must get the chance to ask")
	}
}

func TestAddSessionDirectoryRefusesBroadDirectories(t *testing.T) {
	policy := &Policy{}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	for _, dir := range []string{"/", home, "~"} {
		if _, err := policy.AddSessionDirectory(dir, DirectoryRead); err == nil {
			t.Fatalf("AddSessionDirectory(%q) must be refused", dir)
		}
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.AddSessionDirectory(file, DirectoryRead); err == nil {
		t.Fatal("a file must not be granted as a directory")
	}
}
