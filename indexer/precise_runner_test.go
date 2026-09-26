package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/indexer/precise"
)

var preciseEnv = []string{"GOFLAGS=-mod=mod", "GOTOOLCHAIN=local"}

func openPrecise(t *testing.T, root, dir string, quiet time.Duration) *Engine {
	t.Helper()
	e, err := Open(Options{Root: root, Dir: dir, Precise: true, PreciseQuiet: quiet, PreciseEnv: preciseEnv})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// waitPrecise waits until the tier has loaded everything pending.
func waitPrecise(t *testing.T, e *Engine) PreciseStatus {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if st := e.PreciseStatus(); st.State == PreciseReady || st.State == PreciseUnavailable {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("precise tier still %s", e.PreciseStatus().State)
	return PreciseStatus{}
}

func storedHash(e *Engine, path string) uint64 {
	d := e.Precise().Get(filepath.ToSlash(filepath.Dir(path)))
	if d == nil {
		return 0
	}
	return d.Files[path].Hash
}

func TestPreciseLoadsAfterQuietPeriod(t *testing.T) {
	if err := precise.Available(); err != nil {
		t.Skip(err)
	}
	root, dir := newWorkspace(t)
	e := openPrecise(t, root, dir, 50*time.Millisecond)
	ctx := context.Background()
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	st := waitPrecise(t, e)
	if st.State != PreciseReady || st.Dirs != 2 || st.LastError != "" {
		t.Fatalf("status = %+v", st)
	}
	sn := e.Snapshot()
	m, _ := sn.Meta("b/b.go")
	sn.Release()
	if storedHash(e, "b/b.go") != m.Hash || storedHash(e, "a/a.go") == 0 {
		t.Fatalf("stored hashes: b %x (indexed %x), a %x", storedHash(e, "b/b.go"), m.Hash, storedHash(e, "a/a.go"))
	}
	// vendor/ and testdata/ are not packages of the module.
	if e.Precise().Get("vendor/x") != nil || e.Precise().Get("a/testdata") != nil {
		t.Error("ignored directories loaded")
	}

	// An edit reloads its directory only.
	aBefore := e.Precise().Get("a")
	writeFile(t, root, "b/b.go", "package b\n\nimport \"example.com/ws/a\"\n\ntype T struct{}\n\nfunc (t *T) Run() int { return a.Helper() + 1 }\n")
	if _, err := e.Update(ctx, []string{"b/b.go"}); err != nil {
		t.Fatal(err)
	}
	if got := e.PreciseStatus().State; got != PreciseWaiting && got != PreciseLoading && got != PreciseReady {
		t.Errorf("after an edit: %s", got)
	}
	time.Sleep(100 * time.Millisecond)
	waitPrecise(t, e)
	sn = e.Snapshot()
	m, _ = sn.Meta("b/b.go")
	sn.Release()
	if storedHash(e, "b/b.go") != m.Hash {
		t.Error("edited file not reloaded")
	}
	if e.Precise().Get("a") != aBefore {
		t.Error("unchanged directory reloaded")
	}

	// A new process compares the store with the index and loads nothing.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e2 := openPrecise(t, root, dir, time.Hour)
	if _, err := e2.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e2.RunPrecise(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e2.PreciseStatus(); st.Dirs != 0 || st.State != PreciseReady {
		t.Errorf("reopened: %+v", st)
	}
}

func TestPreciseWithoutToolchain(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root, dir := newWorkspace(t)
	e := openPrecise(t, root, dir, time.Hour)
	ctx := context.Background()
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.RunPrecise(ctx); !errors.Is(err, precise.ErrNoToolchain) {
		t.Fatalf("RunPrecise = %v", err)
	}
	if st := e.Status(); st.Precise.State != PreciseUnavailable || st.LastError != nil {
		t.Errorf("status = %+v", st)
	}
	if _, err := e.Update(ctx, []string{"a/a.go"}); err != nil {
		t.Fatal(err)
	}
}

// A load never delays indexing: edits publish while a (hanging) go
// command runs, and Close stops it.
func TestPreciseNeverBlocksEdits(t *testing.T) {
	fake := t.TempDir()
	script := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(fake, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	root, dir := newWorkspace(t)
	e := openPrecise(t, root, dir, 10*time.Millisecond)
	ctx := context.Background()
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.PreciseStatus().State != PreciseLoading && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.PreciseStatus().State != PreciseLoading {
		t.Fatalf("no load started: %+v", e.PreciseStatus())
	}
	for i := 0; i < 5; i++ {
		writeFile(t, root, "a/a.go", "package a\n\n// Helper helps.\nfunc Helper() int { return "+string(rune('1'+i))+" }\n")
		start := time.Now()
		if _, err := e.Update(ctx, []string{"a/a.go"}); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("edit took %s during a load", d)
		}
	}
	done := make(chan error, 1)
	go func() { done <- e.Close() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not stop the load")
	}
}
