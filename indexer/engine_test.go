package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/indexer/store"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newWorkspace(t *testing.T) (root, dir string) {
	t.Helper()
	base := t.TempDir()
	if c, err := filepath.EvalSymlinks(base); err == nil {
		base = c
	}
	root, dir = filepath.Join(base, "ws"), filepath.Join(base, "index")
	writeFile(t, root, "go.mod", "module example.com/ws\n\ngo 1.22\n")
	writeFile(t, root, "a/a.go", "package a\n\n// Helper helps.\nfunc Helper() int { return 1 }\n")
	writeFile(t, root, "b/b.go", "package b\n\nimport \"example.com/ws/a\"\n\ntype T struct{}\n\nfunc (t *T) Run() int { return a.Helper() }\n")
	writeFile(t, root, "vendor/x/x.go", "package x\n\nfunc Vendored() {}\n")
	writeFile(t, root, "a/testdata/fixture.go", "package fixture\n")
	return root, dir
}

func openEngine(t *testing.T, root, dir string) *Engine {
	t.Helper()
	e, err := Open(Options{Root: root, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func sortedPaths(sn *store.Snapshot) []string {
	p := sn.Paths()
	sort.Strings(p)
	return p
}

// symbol finds the live symbol name and returns "package:signature".
func symbol(sn *store.Snapshot, name string) []string {
	var out []string
	for i := 0; i < sn.NumSegments(); i++ {
		seg := sn.Segment(i)
		lo, hi := seg.SymbolsNamed(name)
		for k := lo; k < hi; k++ {
			if f := seg.SymbolFile(k); sn.Live(i, f) {
				out = append(out, seg.FileMeta(f).Package+":"+seg.Symbol(k).Signature)
			}
		}
	}
	sort.Strings(out)
	return out
}

func callers(sn *store.Snapshot, callee string) []string {
	var out []string
	for i := 0; i < sn.NumSegments(); i++ {
		seg := sn.Segment(i)
		lo, hi := seg.RefsTo(callee)
		for k := lo; k < hi; k++ {
			c := seg.Ref(k)
			if sn.Live(i, c.File) && c.Enclosing >= 0 {
				out = append(out, seg.Symbol(c.Enclosing).Qualified()+"->"+c.Qualifier)
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestReconcileAndIncrementalUpdates(t *testing.T) {
	root, dir := newWorkspace(t)
	e := openEngine(t, root, dir)
	defer e.Close()
	ctx := context.Background()

	st, err := e.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Full || st.Parsed != 2 {
		t.Fatalf("first reconcile = %+v", st)
	}
	sn := e.Snapshot()
	if got := strings.Join(sortedPaths(sn), ","); got != "a/a.go,b/b.go" {
		t.Fatalf("paths = %s (vendor/testdata must be skipped)", got)
	}
	if got := symbol(sn, "Helper"); len(got) != 1 || got[0] != "example.com/ws/a:func Helper() int" {
		t.Fatalf("Helper = %v", got)
	}
	if got := callers(sn, "Helper"); len(got) != 1 || got[0] != "T.Run->example.com/ws/a" {
		t.Fatalf("callers of Helper = %v", got)
	}
	sn.Release()

	// No changes: nothing parsed, no new generation.
	st, err = e.Reconcile(ctx)
	if err != nil || st.Parsed != 0 || st.Generation != 1 {
		t.Fatalf("noop reconcile = %+v, %v", st, err)
	}

	// Body edit via Update.
	writeFile(t, root, "a/a.go", "package a\n\n// Helper helps.\nfunc Helper() int { return 2 }\n\nfunc Extra() { Helper() }\n")
	st, err = e.Update(ctx, []string{filepath.Join(root, "a/a.go")})
	if err != nil || st.Parsed != 1 || st.Full {
		t.Fatalf("update = %+v, %v", st, err)
	}
	sn = e.Snapshot()
	if got := callers(sn, "Helper"); strings.Join(got, ",") != "Extra->,T.Run->example.com/ws/a" {
		t.Fatalf("callers after edit = %v", got)
	}
	sn.Release()

	// New file, then deletion.
	writeFile(t, root, "c/c.go", "package c\n\nfunc New() {}\n")
	if _, err := e.Update(ctx, []string{"c/c.go"}); err != nil {
		t.Fatal(err)
	}
	sn = e.Snapshot()
	if len(symbol(sn, "New")) != 1 {
		t.Fatal("new file not indexed")
	}
	sn.Release()
	if err := os.Remove(filepath.Join(root, "b/b.go")); err != nil {
		t.Fatal(err)
	}
	st, err = e.Update(ctx, []string{filepath.Join(root, "b/b.go")})
	if err != nil || st.Deleted != 1 {
		t.Fatalf("delete update = %+v, %v", st, err)
	}
	sn = e.Snapshot()
	if got := callers(sn, "Helper"); strings.Join(got, ",") != "Extra->" {
		t.Fatalf("callers after delete = %v", got)
	}
	sn.Release()

	// Removing a whole directory triggers a reconcile that drops its files.
	if err := os.RemoveAll(filepath.Join(root, "c")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Update(ctx, []string{filepath.Join(root, "c")}); err != nil {
		t.Fatal(err)
	}
	sn = e.Snapshot()
	if got := strings.Join(sortedPaths(sn), ","); got != "a/a.go" {
		t.Fatalf("paths after dir removal = %s", got)
	}
	sn.Release()
}

func TestReopenReusesIndex(t *testing.T) {
	root, dir := newWorkspace(t)
	ctx := context.Background()
	e := openEngine(t, root, dir)
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = openEngine(t, root, dir)
	defer e.Close()
	st, err := e.Reconcile(ctx)
	if err != nil || st.Parsed != 0 || st.Full {
		t.Fatalf("reopen reconcile = %+v, %v", st, err)
	}
}

func TestModulePathChangeRebuilds(t *testing.T) {
	root, dir := newWorkspace(t)
	ctx := context.Background()
	e := openEngine(t, root, dir)
	defer e.Close()
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "go.mod", "module example.com/renamed\n\ngo 1.22\n")
	st, err := e.Update(ctx, []string{filepath.Join(root, "go.mod")})
	if err != nil || !st.Full {
		t.Fatalf("module change = %+v, %v", st, err)
	}
	sn := e.Snapshot()
	defer sn.Release()
	if got := symbol(sn, "Helper"); len(got) != 1 || !strings.HasPrefix(got[0], "example.com/renamed/a:") {
		t.Fatalf("Helper after module rename = %v", got)
	}
}

func TestCompactionAfterManyUpdates(t *testing.T) {
	root, dir := newWorkspace(t)
	ctx := context.Background()
	e := openEngine(t, root, dir)
	defer e.Close()
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxOverlays+2; i++ {
		writeFile(t, root, "a/a.go", "package a\n\nfunc Helper() int { return "+strings.Repeat("1", i+1)+" }\n")
		if _, err := e.Update(ctx, []string{"a/a.go"}); err != nil {
			t.Fatal(err)
		}
	}
	e.wg.Wait()
	sn := e.Snapshot()
	defer sn.Release()
	if sn.NumSegments() > maxOverlays {
		t.Fatalf("segments = %d, compaction did not run", sn.NumSegments())
	}
	if got := strings.Join(sortedPaths(sn), ","); got != "a/a.go,b/b.go" {
		t.Fatalf("paths after compaction = %s", got)
	}
}

func TestIndexDirInsideRootRejected(t *testing.T) {
	root, _ := newWorkspace(t)
	if _, err := Open(Options{Root: root, Dir: filepath.Join(root, ".index")}); err == nil {
		t.Fatal("expected error for index dir inside root")
	}
}

func TestWatcherPicksUpEdits(t *testing.T) {
	root, dir := newWorkspace(t)
	ctx := context.Background()
	e := openEngine(t, root, dir)
	defer e.Close()
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	updates := make(chan Stats, 16)
	w, err := e.Watch(ctx, func(st Stats, err error) {
		if err == nil {
			updates <- st
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	writeFile(t, root, "a/new.go", "package a\n\nfunc Watched() {}\n")
	deadline := time.After(10 * time.Second)
	for {
		sn := e.Snapshot()
		found := len(symbol(sn, "Watched")) == 1
		sn.Release()
		if found {
			return
		}
		select {
		case <-updates:
		case <-deadline:
			t.Fatal("watcher did not index the new file")
		}
	}
}

func TestStatusReadyAndSync(t *testing.T) {
	root, dir := newWorkspace(t)
	e := openEngine(t, root, dir)
	defer e.Close()
	select {
	case <-e.Ready():
		t.Fatal("ready before the first reconcile")
	default:
	}
	if st := e.Status(); st.Reconciled || st.Generation != 0 || !st.LastPass.IsZero() {
		t.Fatalf("status before reconcile: %+v", st)
	}
	if err := e.Sync(context.Background()); err != nil {
		t.Fatalf("Sync without a watcher: %v", err)
	}
	if _, err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.Ready():
	default:
		t.Fatal("not ready after reconcile")
	}
	st := e.Status()
	if !st.Reconciled || st.Generation == 0 || st.Files != 2 || st.Pending != 0 || st.Busy || st.LastError != nil || st.LastPass.IsZero() {
		t.Fatalf("status after reconcile: %+v", st)
	}

	w, err := e.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	writeFile(t, root, "a/fresh.go", "package a\n\nfunc Fresh() {}\n")
	deadline := time.Now().Add(5 * time.Second)
	for e.Status().Pending == 0 {
		if time.Now().After(deadline) {
			t.Fatal("watcher never saw the new file")
		}
		time.Sleep(time.Millisecond)
	}
	// Sync applies the pending change now, without waiting for the debounce.
	if err := e.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	sn := e.Snapshot()
	defer sn.Release()
	if got := symbol(sn, "Fresh"); len(got) != 1 {
		t.Fatalf("Sync returned before the change was visible: %v", got)
	}
	if p := e.Status().Pending; p != 0 {
		t.Fatalf("pending after Sync = %d", p)
	}
}

// TestNonGoLanguages indexes pack languages next to Go: each file records
// its language and directory unit, and edits and deletions go through the
// same incremental path.
func TestNonGoLanguages(t *testing.T) {
	root, dir := newWorkspace(t)
	writeFile(t, root, "py/store.py", "import os\n\nclass Store:\n    def save(self):\n        os.sync()\n\ndef helper():\n    Store().save()\n")
	writeFile(t, root, "web/app.ts", "import { helper } from './util';\n\nexport function render(): void {\n  helper();\n}\n")
	writeFile(t, root, "notes.bin", "not code\n")
	e := openEngine(t, root, dir)
	defer e.Close()
	ctx := context.Background()

	st, err := e.Reconcile(ctx)
	if err != nil || st.Parsed != 4 {
		t.Fatalf("reconcile = %+v, %v", st, err)
	}
	sn := e.Snapshot()
	if got := strings.Join(sortedPaths(sn), ","); got != "a/a.go,b/b.go,py/store.py,web/app.ts" {
		t.Fatalf("paths = %s", got)
	}
	if got := symbol(sn, "save"); len(got) != 1 || got[0] != "py:def save(self)" {
		t.Fatalf("save = %v", got)
	}
	if got := callers(sn, "helper"); len(got) != 1 || got[0] != "render->" {
		t.Fatalf("callers of helper = %v", got)
	}
	if got := callers(sn, "sync"); len(got) != 1 || got[0] != "Store.save->os" {
		t.Fatalf("callers of sync = %v", got)
	}
	ref, _ := sn.Lookup("web/app.ts")
	if m := sn.Segment(int(ref.Seg)).FileMeta(int(ref.File)); m.Lang != "typescript" || m.Package != "web" {
		t.Fatalf("app.ts meta = %+v", m)
	}
	sn.Release()

	writeFile(t, root, "web/app.ts", "export function render(): void {}\nexport function extra(): void { render(); }\n")
	if st, err = e.Update(ctx, []string{filepath.Join(root, "web/app.ts")}); err != nil || st.Parsed != 1 {
		t.Fatalf("update = %+v, %v", st, err)
	}
	sn = e.Snapshot()
	if got := callers(sn, "helper"); len(got) != 0 {
		t.Fatalf("callers of helper after edit = %v", got)
	}
	if got := callers(sn, "render"); len(got) != 1 || got[0] != "extra->" {
		t.Fatalf("callers of render = %v", got)
	}
	sn.Release()

	if err := os.Remove(filepath.Join(root, "py/store.py")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Update(ctx, []string{"py/store.py"}); err != nil {
		t.Fatal(err)
	}
	sn = e.Snapshot()
	defer sn.Release()
	if len(symbol(sn, "save")) != 0 {
		t.Fatal("deleted python file still indexed")
	}
}
