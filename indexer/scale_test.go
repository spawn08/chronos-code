package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/indexer/store"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitWorkspace(t *testing.T) (root, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, dir = newWorkspace(t)
	run(t, root, "init", "-q", "-b", "main")
	run(t, root, "add", "-A")
	run(t, root, "commit", "-q", "-m", "init")
	return root, dir
}

func has(sn *store.Snapshot, name string) bool { return len(symbol(sn, name)) > 0 }

// After a restart, a complete index reconciles from git: the commit diff,
// the working tree's changes and paths touched since — nothing else is read.
func TestGitReconcileIndexesOnlyChanges(t *testing.T) {
	root, dir := gitWorkspace(t)
	e := openEngine(t, root, dir)
	st, err := e.Reconcile(context.Background())
	if err != nil || st.Mode != "build" {
		t.Fatalf("first reconcile: %+v %v", st, err)
	}
	// An edit applied while running, then reverted to HEAD while stopped:
	// only Touched can reveal it.
	writeFile(t, root, "a/a.go", "package a\n\nfunc Helper() int { return 2 }\n\nfunc Temp() {}\n")
	if _, err := e.Update(context.Background(), []string{"a/a.go"}); err != nil {
		t.Fatal(err)
	}
	e.Close()
	run(t, root, "checkout", "--", "a/a.go")
	// Committed, staged-free worktree edit, untracked file, deletion.
	writeFile(t, root, "c/c.go", "package c\n\nfunc Committed() {}\n")
	run(t, root, "add", "c/c.go")
	run(t, root, "commit", "-q", "-m", "c")
	writeFile(t, root, "b/b.go", "package b\n\nfunc Edited() {}\n")
	writeFile(t, root, "d/new.go", "package d\n\nfunc Untracked() {}\n")
	run(t, root, "rm", "-q", "--cached", "c/c.go")
	if err := os.Remove(filepath.Join(root, "c", "c.go")); err != nil {
		t.Fatal(err)
	}

	e = openEngine(t, root, dir)
	defer e.Close()
	st, err = e.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != "git" || st.Scanned > 4 {
		t.Fatalf("git reconcile: %+v", st)
	}
	sn := e.Snapshot()
	defer sn.Release()
	for name, want := range map[string]bool{"Temp": false, "Helper": true, "Edited": true, "Run": false, "Untracked": true, "Committed": false} {
		if has(sn, name) != want {
			t.Fatalf("%s present=%v, want %v", name, !want, want)
		}
	}
	if m := e.Manifest().Meta; m.Commit == "" || !m.Complete {
		t.Fatalf("meta: %+v", m)
	}

	// A module change cannot be answered from git: it lists.
	writeFile(t, root, "go.mod", "module example.com/renamed\n\ngo 1.22\n")
	if st, err := e.Reconcile(context.Background()); err != nil || st.Mode == "git" {
		t.Fatalf("module change reconcile: %+v %v", st, err)
	}
}

// A branch switch while stopped is reconciled from the commit diff.
func TestGitReconcileBranchSwitch(t *testing.T) {
	root, dir := gitWorkspace(t)
	run(t, root, "checkout", "-q", "-b", "feature")
	writeFile(t, root, "a/feature.go", "package a\n\nfunc Feature() {}\n")
	run(t, root, "add", "-A")
	run(t, root, "commit", "-q", "-m", "feature")
	e := openEngine(t, root, dir)
	if _, err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.Close()
	run(t, root, "checkout", "-q", "main")
	e = openEngine(t, root, dir)
	defer e.Close()
	st, err := e.Reconcile(context.Background())
	if err != nil || st.Mode != "git" || st.Deleted != 1 {
		t.Fatalf("branch switch: %+v %v", st, err)
	}
	sn := e.Snapshot()
	defer sn.Release()
	if has(sn, "Feature") {
		t.Fatal("feature branch symbol still indexed")
	}
}

// A touched file whose content is unchanged is re-recorded from its stored
// facts without parsing.
func TestUnchangedContentIsReused(t *testing.T) {
	root, dir := newWorkspace(t)
	e := openEngine(t, root, dir)
	defer e.Close()
	if _, err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "a", "a.go")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	st, err := e.Update(context.Background(), []string{p})
	if err != nil || st.Reused != 1 || st.Parsed != 0 {
		t.Fatalf("touch: %+v %v", st, err)
	}
	st, err = e.Update(context.Background(), []string{p})
	if err != nil || st.Reused+st.Parsed != 0 {
		t.Fatalf("stat must match after reuse: %+v %v", st, err)
	}
	sn := e.Snapshot()
	defer sn.Release()
	if !has(sn, "Helper") {
		t.Fatal("reused facts lost")
	}
}

// A large first build publishes the working set first (partial coverage),
// then the complete base.
func TestProgressiveFirstBuild(t *testing.T) {
	root, dir := gitWorkspace(t)
	writeFile(t, root, "focus/f.go", "package focus\n\nfunc Focused() {}\n")
	writeFile(t, root, "b/b.go", "package b\n\nfunc Dirty() {}\n")
	e, err := Open(Options{Root: root, Dir: dir, ProgressiveFiles: 1, Focus: "focus"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	listing := []string{"a/a.go", "b/b.go", "focus/f.go"}
	dirty, _ := gitDirty(context.Background(), root)
	e.modules = map[string]string{".": "example.com/ws"}
	e.publishWorkingSet(context.Background(), listing, dirty)
	sn := e.Snapshot()
	// (a/a.go is in the working set too: the initial commit touched it.)
	if !has(sn, "Focused") || !has(sn, "Dirty") || e.Status().Complete || sn.NumShards() != 0 {
		t.Fatalf("working set: files=%d complete=%v", sn.NumFiles(), e.Status().Complete)
	}
	sn.Release()
	if _, err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	sn = e.Snapshot()
	defer sn.Release()
	if !has(sn, "Helper") || !has(sn, "Focused") || !e.Status().Complete || sn.NumSegments() != sn.NumShards() {
		t.Fatalf("after build: files=%d complete=%v segments=%d", sn.NumFiles(), e.Status().Complete, sn.NumSegments())
	}
}

// A prebuilt index for another checkout of the repository is imported, and
// reconciling indexes only the commits and edits since it was built.
func TestImportPrebuiltIndex(t *testing.T) {
	ci, ciDir := gitWorkspace(t)
	builder := openEngine(t, ci, ciDir)
	if _, err := builder.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder.Close()

	base := t.TempDir()
	local := filepath.Join(base, "local")
	run(t, base, "clone", "-q", ci, local)
	writeFile(t, local, "a/later.go", "package a\n\nfunc Later() {}\n")
	run(t, local, "add", "-A")
	run(t, local, "commit", "-q", "-m", "later")

	e := openEngine(t, local, filepath.Join(base, "index"))
	defer e.Close()
	if err := e.Import(ciDir); err != nil {
		t.Fatal(err)
	}
	st, err := e.Reconcile(context.Background())
	if err != nil || st.Mode != "git" || st.Parsed+st.Reused > 1 {
		t.Fatalf("reconcile after import: %+v %v", st, err)
	}
	sn := e.Snapshot()
	defer sn.Release()
	if !has(sn, "Later") || !has(sn, "Helper") || e.Manifest().Root != local {
		t.Fatalf("imported index: files=%d", sn.NumFiles())
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPollBackendPicksUpEditsAndCommits(t *testing.T) {
	root, dir := gitWorkspace(t)
	e, err := Open(Options{Root: root, Dir: dir, WatchBackend: "poll", PollInterval: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, err := e.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.Backend() != "poll" || e.Status().Backend != "poll" {
		t.Fatalf("backend = %s", w.Backend())
	}
	symbolNow := func(name string) bool { sn := e.Snapshot(); defer sn.Release(); return has(sn, name) }
	writeFile(t, root, "a/polled.go", "package a\n\nfunc Polled() {}\n")
	waitFor(t, "untracked file", func() bool { return symbolNow("Polled") })
	run(t, root, "add", "-A")
	run(t, root, "commit", "-q", "-m", "polled")
	writeFile(t, root, "a/polled.go", "package a\n\nfunc Polled2() {}\n")
	waitFor(t, "edit after commit", func() bool { return symbolNow("Polled2") && !symbolNow("Polled") })
	run(t, root, "checkout", "--", "a/polled.go") // revert: file becomes clean
	waitFor(t, "revert", func() bool { return symbolNow("Polled") && !symbolNow("Polled2") })
}

// Auto mode never uses fsnotify above its file limit.
func TestAutoBackendAvoidsFsnotifyOnLargeIndexes(t *testing.T) {
	root, dir := gitWorkspace(t)
	e, err := Open(Options{Root: root, Dir: dir, FsnotifyMaxFiles: 1, PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	gitPath, _ := exec.LookPath("git")
	bin := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin) // git only: no watchman
	w, err := e.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.Backend() != "poll" {
		t.Fatalf("auto backend over the limit = %s, want poll", w.Backend())
	}
}

// The watchman backend speaks the CLI's JSON protocol; a fake watchman
// reports one changed file per query, and Sync polls it before returning.
func TestWatchmanBackendAndSync(t *testing.T) {
	root, dir := newWorkspace(t)
	bin := t.TempDir()
	changes := filepath.Join(bin, "changes")
	script := `#!/bin/sh
in=$(cat)
case "$in" in
  *watch-project*) printf '{"watch":"%s"}' "` + root + `" ;;
  *'"clock"'*) printf '{"clock":"c:1"}' ;;
  *query*) files=$(cat "` + changes + `" 2>/dev/null); : > "` + changes + `"; printf '{"clock":"c:2","files":[%s]}' "$files" ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "watchman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	e, err := Open(Options{Root: root, Dir: dir, WatchBackend: "watchman"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, err := e.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	writeFile(t, root, "a/wm.go", "package a\n\nfunc FromWatchman() {}\n")
	if err := os.WriteFile(changes, []byte(`"a/wm.go"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	sn := e.Snapshot()
	defer sn.Release()
	if !has(sn, "FromWatchman") {
		data, _ := os.ReadFile(changes)
		t.Fatalf("Sync did not apply the watchman change (pending file: %q)", strings.TrimSpace(string(data)))
	}
}
