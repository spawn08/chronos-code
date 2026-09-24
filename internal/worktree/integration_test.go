package worktree

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealRepositoryIsolationCollectionAndIntegration(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	dataDir := filepath.Join(t.TempDir(), "project-data")
	manager, err := New(dataDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	userBytes := []byte("uncommitted parent bytes\x00\xff")
	if err := os.WriteFile(filepath.Join(repo, "user.bin"), userBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	handle, err := manager.Create(ctx, filepath.Join(repo, "sub"), CreateOptions{TaskID: "task-17", AttemptID: "attempt-2", DirtyPolicy: DirtyPreserve})
	if err != nil {
		t.Fatal(err)
	}
	if handle.Manifest.RepoRoot != canonical(t, repo) || len(handle.Manifest.ParentDirtyPaths) != 1 || handle.Manifest.ParentDirtyPaths[0] != "user.bin" {
		t.Fatalf("captured manifest = %+v", handle.Manifest)
	}
	if _, err := os.Stat(filepath.Join(handle.Manifest.WorktreePath, "user.bin")); !os.IsNotExist(err) {
		t.Fatalf("parent untracked file leaked into worktree: %v", err)
	}
	binary := []byte{0, 1, 2, 255, '\n'}
	if err := os.WriteFile(filepath.Join(handle.Manifest.WorktreePath, "artifact.bin"), binary, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(handle.Manifest.WorktreePath, "tracked.txt"), []byte("isolated\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := manager.Collect(ctx, handle, []Check{{Name: "review", Passed: true}})
	if err != nil {
		t.Fatal(err)
	}
	if result.BaseRevision == "" || result.FinalTree == "" || result.FinalHash == "" || !bytes.Contains(result.Patch, []byte("GIT binary patch")) {
		t.Fatalf("incomplete result: %+v patch=%q", result, result.Patch)
	}
	if !reflectStrings(result.ChangedPaths, []string{"artifact.bin", "tracked.txt"}) {
		t.Fatalf("changed paths = %q", result.ChangedPaths)
	}

	integrated, err := manager.Integrate(ctx, handle, []string{"tracked.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if integrated.Cleanup.State != "complete" || !reflectStrings(integrated.ChangedPaths, []string{"tracked.txt"}) {
		t.Fatalf("integration result = %+v", integrated)
	}
	got, err := os.ReadFile(filepath.Join(repo, "tracked.txt"))
	if err != nil || string(got) != "isolated\n" {
		t.Fatalf("tracked.txt = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "artifact.bin")); !os.IsNotExist(err) {
		t.Fatalf("unselected path was applied: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(repo, "user.bin"))
	if err != nil || !bytes.Equal(got, userBytes) {
		t.Fatalf("parent user bytes changed: %x, %v", got, err)
	}
	if recovered, err := manager.Recover(); err != nil || len(recovered) != 0 {
		t.Fatalf("successful cleanup left manifests: %+v, %v", recovered, err)
	}
}

func TestDependentWorktreeCharacterizesMissingPredecessorSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	manager, err := New(filepath.Join(t.TempDir(), "project-data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err := manager.Create(ctx, repo, CreateOptions{TaskID: "task", AttemptID: "predecessor", DirtyPolicy: DirtyPreserve})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(predecessor.Manifest.WorktreePath, "tracked.txt"), []byte("accepted predecessor\n"))
	if _, err := manager.Integrate(ctx, predecessor, []string{"tracked.txt"}); err != nil {
		t.Fatal(err)
	}

	dependent, err := manager.Create(ctx, repo, CreateOptions{TaskID: "task", AttemptID: "dependent", DirtyPolicy: DirtyPreserve})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Cancel(context.Background(), dependent) })
	got, err := os.ReadFile(filepath.Join(dependent.Manifest.WorktreePath, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "base\n" {
		t.Fatalf("dependent content = %q, want current HEAD characterization", got)
	}
	if parent, err := os.ReadFile(filepath.Join(repo, "tracked.txt")); err != nil || string(parent) != "accepted predecessor\n" {
		t.Fatalf("parent predecessor content = %q, %v", parent, err)
	}
}

func TestIntegrationConflictsLeaveParentBytesUnchanged(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, string, Handle)
		wantErr string
	}{
		{
			name: "dirty overlap",
			prepare: func(t *testing.T, repo string, _ Handle) {
				t.Helper()
				mustWrite(t, filepath.Join(repo, "tracked.txt"), []byte("user edit\n"))
			},
			wantErr: "overlaps selected path",
		},
		{
			name: "stale base",
			prepare: func(t *testing.T, repo string, _ Handle) {
				t.Helper()
				mustWrite(t, filepath.Join(repo, "other.txt"), []byte("new commit\n"))
				gitTest(t, repo, "add", "other.txt")
				gitTest(t, repo, "commit", "-m", "advance")
			},
			wantErr: "stale base",
		},
		{
			name: "apply check",
			prepare: func(t *testing.T, repo string, _ Handle) {
				t.Helper()
				mustWrite(t, filepath.Join(repo, "tracked.txt"), []byte("hidden parent edit\n"))
				gitTest(t, repo, "update-index", "--assume-unchanged", "tracked.txt")
			},
			wantErr: "selected patch conflicts with parent",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newTestRepo(t)
			manager, err := New(filepath.Join(t.TempDir(), "data"), nil)
			if err != nil {
				t.Fatal(err)
			}
			handle, err := manager.Create(context.Background(), repo, CreateOptions{TaskID: "t", AttemptID: test.name, DirtyPolicy: DirtyReject})
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(handle.Manifest.WorktreePath, "tracked.txt"), []byte("attempt edit\n"))
			test.prepare(t, repo, handle)
			before := snapshotFiles(t, repo)
			_, err = manager.Integrate(context.Background(), handle, []string{"tracked.txt"})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Integrate() error = %v", err)
			}
			after := snapshotFiles(t, repo)
			if !bytes.Equal(before, after) {
				t.Fatalf("parent bytes changed on conflict\nbefore=%x\nafter=%x", before, after)
			}
		})
	}
}

func TestIntegrationRejectsParentSymlinkEscapeAndCancelCleans(t *testing.T) {
	repo := newTestRepo(t)
	manager, err := New(filepath.Join(t.TempDir(), "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Create(context.Background(), repo, CreateOptions{TaskID: "t", AttemptID: "a", DirtyPolicy: DirtyReject})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(handle.Manifest.WorktreePath, "escape", "file.txt"), []byte("attempt\n"))
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "escape")); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Integrate(context.Background(), handle, []string{"escape/file.txt"})
	if err == nil || !strings.Contains(err.Error(), "traverses symlink") {
		t.Fatalf("Integrate() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("integration escaped repository: %v", err)
	}
	if err := os.Remove(filepath.Join(repo, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := manager.Cancel(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	if fileExists(handle.Manifest.WorktreePath) || fileExists(handle.Manifest.ManifestPath) {
		t.Fatal("cancel left private worktree or manifest")
	}
}

func TestRecoverAndPruneCrashArtifacts(t *testing.T) {
	repo := newTestRepo(t)
	manager, err := New(filepath.Join(t.TempDir(), "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Create(context.Background(), repo, CreateOptions{TaskID: "crash", AttemptID: "1", DirtyPolicy: DirtyReject})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := manager.Recover()
	if err != nil || len(recovered) != 1 || recovered[0].Manifest.ID != handle.Manifest.ID {
		t.Fatalf("Recover() = %+v, %v", recovered, err)
	}
	if err := os.RemoveAll(handle.Manifest.WorktreePath); err != nil {
		t.Fatal(err)
	}
	if err := manager.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recovered, err := manager.Recover(); err != nil || len(recovered) != 0 {
		t.Fatalf("Prune() left recovery records: %+v, %v", recovered, err)
	}
	command := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", handle.Manifest.Ref)
	if err := command.Run(); err == nil {
		t.Fatalf("Prune() left ref %s", handle.Manifest.Ref)
	}
}

func newTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitTest(t, repo, "init")
	gitTest(t, repo, "config", "user.name", "Chronos Test")
	gitTest(t, repo, "config", "user.email", "chronos@example.invalid")
	mustWrite(t, filepath.Join(repo, "tracked.txt"), []byte("base\n"))
	if err := os.Mkdir(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "tracked.txt")
	gitTest(t, repo, "commit", "-m", "base")
	return repo
}

func gitTest(t *testing.T, repo string, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return output
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	result, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func reflectStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func snapshotFiles(t *testing.T, repo string) []byte {
	t.Helper()
	var snapshot bytes.Buffer
	err := filepath.WalkDir(repo, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(repo, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot.WriteString(relative)
		snapshot.WriteByte(0)
		snapshot.Write(data)
		snapshot.WriteByte(0)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.Bytes()
}
