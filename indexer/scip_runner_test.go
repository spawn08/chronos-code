package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/indexer/scip"
	"github.com/spawn08/chronos-code/indexer/scip/sciptest"
)

const (
	scipLib  = "export function greet(): string {\n  return \"hi\";\n}\n"
	scipMain = "import { greet } from \"./lib\";\n\nexport const s = greet();\n"
	scipSym  = "scip-typescript npm app 1.0.0 `web/lib.ts`/greet()."
)

// scipWorkspace writes a TypeScript workspace under web/ (files dated an
// hour ago) and returns its root, an index directory, and a prebuilt
// index of it outside the workspace.
func scipWorkspace(t *testing.T) (root, dir, prebuilt string) {
	t.Helper()
	base := t.TempDir()
	if c, err := filepath.EvalSymlinks(base); err == nil {
		base = c
	}
	root, dir = filepath.Join(base, "ws"), filepath.Join(base, "index")
	old := time.Now().Add(-time.Hour)
	for name, src := range map[string]string{"web/lib.ts": scipLib, "web/main.ts": scipMain} {
		writeFile(t, root, name, src)
		_ = os.Chtimes(filepath.Join(root, filepath.FromSlash(name)), old, old)
	}
	prebuilt = filepath.Join(base, "prebuilt.scip")
	err := sciptest.Write(prebuilt, "file:///ci", sciptest.Document{Path: "web/lib.ts", Language: "typescript", Occurrences: []sciptest.Occurrence{
		sciptest.At(scipLib, "greet", 0, scipSym, scip.RoleDefinition),
	}}, sciptest.Document{Path: "web/main.ts", Language: "typescript", Occurrences: []sciptest.Occurrence{
		sciptest.At(scipMain, "greet", 0, scipSym, scip.RoleImport),
		sciptest.At(scipMain, "greet", 1, scipSym, 0),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return root, dir, prebuilt
}

func openSCIP(t *testing.T, opts Options) *Engine {
	t.Helper()
	opts.SCIP = true
	e, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func waitSCIP(t *testing.T, e *Engine, ok func(SCIPStatus) bool) SCIPStatus {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st := e.SCIPStatus(); ok(st) {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("SCIP tier: %+v", e.SCIPStatus())
	return SCIPStatus{}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSCIPImportsDiscoveredIndex(t *testing.T) {
	root, dir, prebuilt := scipWorkspace(t)
	e := openSCIP(t, Options{Root: root, Dir: dir, SCIPPoll: 20 * time.Millisecond})
	ctx := context.Background()
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if e.SCIP().Get("web") != nil {
		t.Fatal("facts without an index")
	}
	// An index appearing at the root is noticed and imported.
	copyFile(t, prebuilt, filepath.Join(root, "index.scip"))
	st := waitSCIP(t, e, func(s SCIPStatus) bool { return s.Docs == 2 && s.State == SCIPReady })
	if st.Rejected != 0 || st.Indexes != 1 || st.LastError != "" {
		t.Fatalf("status %+v", st)
	}
	d := e.SCIP().Get("web")
	if d == nil || len(d.Files["web/main.ts"].Calls) != 1 {
		t.Fatalf("facts %+v", d)
	}
	_ = e.Close()

	// Reopened with current facts: nothing is imported again.
	e = openSCIP(t, Options{Root: root, Dir: dir, SCIPPoll: 20 * time.Millisecond})
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // past the import quiet period
	if st := e.SCIPStatus(); st.LastImport != 0 {
		t.Fatalf("reimported: %+v", st)
	}
	if e.SCIP().Get("web") == nil {
		t.Fatal("facts lost on reopen")
	}

	// Removing the index removes its facts.
	if err := os.Remove(filepath.Join(root, "index.scip")); err != nil {
		t.Fatal(err)
	}
	waitSCIP(t, e, func(s SCIPStatus) bool { return s.LastImport > 0 && s.Docs == 0 })
	if e.SCIP().Get("web") != nil {
		t.Fatal("facts of a removed index kept")
	}
}

func TestSCIPCommandRunIsGatedExactly(t *testing.T) {
	root, dir, prebuilt := scipWorkspace(t)
	// lib.ts is newer than any index could be: the check for indexes built
	// elsewhere would leave it out, a watched run does not need it.
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(root, "web", "lib.ts"), future, future)
	e := openSCIP(t, Options{Root: root, Dir: dir, SCIPPoll: time.Hour, SCIPQuiet: 50 * time.Millisecond,
		SCIPSources: []SCIPSource{{Index: "web/out.scip", Command: "cp '" + prebuilt + "' web/out.scip"}}})
	ctx := context.Background()
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	// The index is missing: the command runs after the quiet period.
	st := waitSCIP(t, e, func(s SCIPStatus) bool { return s.Docs == 2 && s.State == SCIPReady })
	if st.Rejected != 0 || st.LastError != "" {
		t.Fatalf("status %+v", st)
	}
	if _, err := os.Stat(filepath.Join(root, "web", "out.scip")); err != nil {
		t.Fatal(err)
	}
	r := e.scip
	r.mu.Lock()
	last := r.state.Runs[filepath.Join(root, "web", "out.scip")]
	r.mu.Unlock()
	if last == nil || len(last.Observed) != 2 || len(last.Exts) != 1 || last.Exts[0] != ".ts" {
		t.Fatalf("run %+v", last)
	}
	// Nothing covered changed: not due.
	if due, _ := r.commandDue(r.sources[0], true); due {
		t.Error("command due without changes")
	}
	// A covered file changing makes it due.
	writeFile(t, root, "web/main.ts", scipMain+"// more\n")
	if _, err := e.Update(ctx, []string{"web/main.ts"}); err != nil {
		t.Fatal(err)
	}
	if due, _ := r.commandDue(r.sources[0], true); !due {
		t.Error("command not due after a covered file changed")
	}
}

func TestSCIPEditsDuringARunAreNotBlocked(t *testing.T) {
	root, dir, prebuilt := scipWorkspace(t)
	started := filepath.Join(t.TempDir(), "started")
	e := openSCIP(t, Options{Root: root, Dir: dir, SCIPPoll: time.Hour, SCIPQuiet: time.Hour,
		SCIPSources: []SCIPSource{{Index: "web/out.scip",
			Command: "touch '" + started + "'; sleep 1; cp '" + prebuilt + "' web/out.scip"}}})
	ctx := context.Background()
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.RunSCIP(ctx) }()
	for i := 0; ; i++ {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if i > 500 {
			t.Fatal("command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// An edit while the command runs is applied at once...
	writeFile(t, root, "web/main.ts", scipMain+"export const t = greet();\n")
	begin := time.Now()
	if _, err := e.Update(ctx, []string{"web/main.ts"}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > 500*time.Millisecond {
		t.Errorf("edit took %v during a run", d)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// ...and its file's document is left out: the run may have read either
	// version. The unchanged file is imported.
	st := e.SCIPStatus()
	if st.Docs != 1 || st.Rejected != 1 {
		t.Fatalf("status %+v", st)
	}
	d := e.SCIP().Get("web")
	if _, ok := d.Files["web/main.ts"]; ok {
		t.Error("edited file imported")
	}
	if _, ok := d.Files["web/lib.ts"]; !ok {
		t.Error("unchanged file not imported")
	}
}

func TestSCIPCloseStopsARun(t *testing.T) {
	root, dir, _ := scipWorkspace(t)
	e := openSCIP(t, Options{Root: root, Dir: dir, SCIPPoll: time.Hour, SCIPQuiet: time.Hour,
		SCIPSources: []SCIPSource{{Index: "out.scip", Command: "sleep 30"}}})
	ctx := context.Background()
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.RunSCIP(ctx) }()
	time.Sleep(200 * time.Millisecond)
	begin := time.Now()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > 5*time.Second {
		t.Fatalf("Close took %v", d)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("run: %v", err)
	}
}

func TestSCIPFailingCommandIsReported(t *testing.T) {
	root, dir, _ := scipWorkspace(t)
	e := openSCIP(t, Options{Root: root, Dir: dir, SCIPPoll: time.Hour, SCIPQuiet: time.Hour,
		SCIPSources: []SCIPSource{{Index: "out.scip", Command: "echo boom >&2; exit 3"}}})
	ctx := context.Background()
	if _, err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.RunSCIP(ctx); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("run: %v", err)
	}
	if st := e.SCIPStatus(); !strings.Contains(st.LastError, "boom") || st.State != SCIPReady {
		t.Fatalf("status %+v", st)
	}
}
