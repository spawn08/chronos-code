package graph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
)

var payFiles = map[string]string{
	"go.mod": "module pay\n\ngo 1.24\n",
	"service.go": `package pay

// Payer charges money.
type Payer interface{ Pay(amount int) error }

type Card struct{}

// Pay charges the card.
func (Card) Pay(amount int) error { return record(amount) }

func record(amount int) error { return nil }
`,
	"handler.go":      "package pay\n\nfunc Handle(p Payer) error { return p.Pay(1) }\n",
	"handler_test.go": "package pay\n\nimport \"testing\"\n\nfunc TestHandle(t *testing.T) { _ = Handle(Card{}) }\n",
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func newTestScope(t *testing.T, root string, watch bool) *IndexScope {
	t.Helper()
	s, err := NewIndexScope(context.Background(), IndexScopeOptions{Root: root, DataDir: t.TempDir(), IndexOnStart: true, Watch: watch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func toolFrom(t *testing.T, defs []*tool.Definition, name string) *tool.Definition {
	t.Helper()
	for _, d := range defs {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("tool %s not found", name)
	return nil
}

func call(t *testing.T, ctx context.Context, def *tool.Definition, args map[string]any) map[string]any {
	t.Helper()
	out, err := def.Handler(ctx, args)
	if err != nil {
		t.Fatalf("%s(%v): %v", def.Name, args, err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("%s returned %T", def.Name, out)
	}
	return m
}

func withoutIndex(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != "index" {
			out[k] = v
		}
	}
	return out
}

func TestIndexScopeBatchedNamesMatchSeparateCalls(t *testing.T) {
	root := canonicalTempDir(t)
	writeTree(t, root, payFiles)
	scope := newTestScope(t, root, false)
	ctx := context.Background()
	for tool, names := range map[string][]string{
		"graph_query":          {"Pay", "Handle", "Missing"},
		"find_callers":         {"record", "Pay", "Missing"},
		"find_implementations": {"Payer", "Missing"},
		"resolve_symbol":       {"Card", "Pay", "Missing"},
	} {
		def := toolFrom(t, scope.Tools(), tool)
		if req, ok := def.Parameters["required"]; ok {
			t.Fatalf("%s: batched tools must not require name: %v", tool, req)
		}
		batch := call(t, ctx, def, map[string]any{"names": names, "depth": 2})
		results := batch["results"].([]map[string]any)
		if len(results) != len(names) || batch["index"] == nil {
			t.Fatalf("%s batch: %+v", tool, batch)
		}
		for i, name := range names {
			single := call(t, ctx, def, map[string]any{"name": name, "depth": 2})
			if results[i]["name"] != name || !reflect.DeepEqual(results[i]["result"], withoutIndex(single)) {
				t.Fatalf("%s(%s): batch %v != single %v", tool, name, results[i]["result"], withoutIndex(single))
			}
		}
	}
	if _, err := toolFrom(t, scope.Tools(), "graph_query").Handler(ctx, map[string]any{"names": []any{1}}); err == nil {
		t.Fatal("non-string names must be rejected")
	}
	many := make([]any, maxBatchNames+1)
	for i := range many {
		many[i] = fmt.Sprintf("N%d", i)
	}
	if _, err := toolFrom(t, scope.Tools(), "graph_query").Handler(ctx, map[string]any{"names": many}); err == nil {
		t.Fatal("oversized batch must be rejected")
	}
}

func TestIndexScopeLabelsEmptyResults(t *testing.T) {
	root := canonicalTempDir(t)
	writeTree(t, root, payFiles)
	scope := newTestScope(t, root, false)
	ctx := context.Background()
	q := call(t, ctx, toolFrom(t, scope.Tools(), "graph_query"), map[string]any{"name": "Missing"})
	index, _ := q["index"].(map[string]any)
	if q["found"] != false || index == nil || index["up_to_date"] != true || index["mode"] != "syntactic" || index["files"] != 3 {
		t.Fatalf("graph_query label: %+v", q)
	}
	if note, _ := q["note"].(string); !strings.Contains(note, `No declaration named "Missing"`) || !strings.Contains(note, "index up to date") {
		t.Fatalf("graph_query note: %q", note)
	}
	// Each caller carries its own label and call site; a test caller is marked.
	c := call(t, ctx, toolFrom(t, scope.Tools(), "find_callers"), map[string]any{"name": "Handle"})
	levels, _ := c["callers_by_depth"].([]map[string]map[string][]string)
	if c["note"] != nil || len(levels) != 1 || !reflect.DeepEqual(levels[0]["Handle"], map[string][]string{"import_resolved": {"TestHandle (handler_test.go:5)"}}) {
		t.Fatalf("non-empty callers must be listed without a note: %+v", c)
	}
	c = call(t, ctx, toolFrom(t, scope.Tools(), "find_callers"), map[string]any{"name": "TestHandle"})
	if note, _ := c["note"].(string); !strings.Contains(note, `No callers of "TestHandle"`) {
		t.Fatalf("empty callers label: %+v", c)
	}
	i := call(t, ctx, toolFrom(t, scope.Tools(), "find_implementations"), map[string]any{"name": "Nope"})
	if note, _ := i["note"].(string); !strings.Contains(note, `No interface named "Nope"`) {
		t.Fatalf("missing interface note: %+v", i)
	}
	s := call(t, ctx, toolFrom(t, scope.Tools(), "codebase_search"), map[string]any{"query": "zzqx"})
	if s["found"] != false || s["note"] == nil || s["index"] == nil {
		t.Fatalf("empty search label: %+v", s)
	}
	// A backend that does not report freshness leaves the output unchanged.
	plain := call(t, ctx, toolFrom(t, Tools(newMemBackend(), root), "graph_query"), map[string]any{"name": "Missing"})
	if !reflect.DeepEqual(plain, map[string]any{"found": false}) {
		t.Fatalf("store output changed: %+v", plain)
	}
}

// No query path may run the Go toolchain (go list / packages.Load): a fake
// go binary first on PATH records any invocation.
func TestIndexScopeNeverRunsGo(t *testing.T) {
	fake := t.TempDir()
	marker := filepath.Join(fake, "invoked")
	script := "#!/bin/sh\necho \"$@\" >> " + marker + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(fake, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := canonicalTempDir(t)
	writeTree(t, root, payFiles)
	scope := newTestScope(t, root, true)
	ctx := context.Background()
	args := map[string]map[string]any{
		"graph_query":           {"name": "Pay"},
		"codebase_search":       {"query": "pay card"},
		"codebase_context":      {"query": "record"},
		"codebase_map":          {"query": "pay"},
		"find_callers":          {"name": "record", "depth": 3},
		"find_implementations":  {"name": "Payer"},
		"multi_resolution_view": {"level": "L2", "target": "Pay"},
		"resolve_symbol":        {"name": "Card"},
		"impact_analysis":       {"file": "service.go", "start_line": 1, "end_line": 20},
		"test_map":              {"symbol": "Pay"},
		"co_change":             {"file": "service.go"},
	}
	run := func() {
		for _, def := range append(scope.Tools(), scope.ImpactTools()...) {
			a, ok := args[def.Name]
			if !ok {
				t.Fatalf("no arguments for tool %s", def.Name)
			}
			if _, err := def.Handler(ctx, a); err != nil {
				t.Fatalf("%s: %v", def.Name, err)
			}
		}
	}
	run()
	// Edits (including a signature change) must not reach the toolchain either.
	writeTree(t, root, map[string]string{"service.go": strings.Replace(payFiles["service.go"], "amount int", "amount int64", -1)})
	if err := scope.Engine().Sync(ctx); err != nil {
		t.Fatal(err)
	}
	run()
	if _, err := scope.Live().Stats(ctx); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("query path ran the go toolchain: %s", data)
	}
}

func TestIndexScopeSeesEditsQuickly(t *testing.T) {
	root := canonicalTempDir(t)
	writeTree(t, root, payFiles)
	scope := newTestScope(t, root, true)
	ctx := context.Background()
	query := toolFrom(t, scope.Tools(), "graph_query")
	if r := call(t, ctx, query, map[string]any{"name": "Refund"}); r["found"] != false {
		t.Fatalf("fixture already has Refund: %+v", r)
	}
	writeTree(t, root, map[string]string{"refund.go": "package pay\n\nfunc Refund() {}\n"})
	start := time.Now()
	for {
		r := call(t, ctx, query, map[string]any{"name": "Refund"})
		if r["found"] == true {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("edit never became visible: %+v", r)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("edit took %s to become visible", elapsed)
	}
}

func TestIndexScopeWorkspaceOverrideAndUnavailable(t *testing.T) {
	parent := canonicalTempDir(t)
	child := filepath.Join(parent, "child")
	writeTree(t, parent, map[string]string{"go.mod": "module parent\n\ngo 1.24\n", "main.go": "package p\n\nfunc ParentOnly() {}\n"})
	writeTree(t, child, map[string]string{"go.mod": "module child\n\ngo 1.24\n", "main.go": "package c\n\nfunc ChildOnly() {}\n"})
	scope := newTestScope(t, parent, false)
	query := toolFrom(t, scope.Tools(), "graph_query")
	ctx := context.Background()
	childCtx := builtins.WithWorkspaceRoot(ctx, child)
	if r := call(t, ctx, query, map[string]any{"name": "ParentOnly"}); r["found"] != true {
		t.Fatalf("parent root: %+v", r)
	}
	if r := call(t, childCtx, query, map[string]any{"name": "ChildOnly"}); r["found"] != true {
		t.Fatalf("child root: %+v", r)
	}
	// The parent's index skips nested modules only if it lists them; the
	// child's index must never contain the parent's files.
	if r := call(t, childCtx, query, map[string]any{"name": "ParentOnly"}); r["found"] != false {
		t.Fatalf("child root leaked parent symbols: %+v", r)
	}
	missing := builtins.WithWorkspaceRoot(ctx, filepath.Join(parent, "does-not-exist"))
	r := call(t, missing, query, map[string]any{"name": "ParentOnly"})
	if r["available"] != false || r["reason"] == nil {
		t.Fatalf("unavailable shape: %+v", r)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := scope.Live().Stats(ctx); err == nil {
		t.Fatal("live backend must fail after Close")
	}
	if r := call(t, ctx, query, map[string]any{"name": "ParentOnly"}); r["available"] != false {
		t.Fatalf("tool after Close: %+v", r)
	}
}

// Two sessions on one repository: the second cannot take the writer lock
// and uses a private index, removed when it closes.
func TestIndexScopeLockedIndexFallsBackToPrivate(t *testing.T) {
	root := canonicalTempDir(t)
	writeTree(t, root, payFiles)
	data := t.TempDir()
	open := func() *IndexScope {
		s, err := NewIndexScope(context.Background(), IndexScopeOptions{Root: root, DataDir: data, IndexOnStart: true})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first, second := open(), open()
	defer first.Close()
	ctx := context.Background()
	for _, s := range []*IndexScope{first, second} {
		if r := call(t, ctx, toolFrom(t, s.Tools(), "graph_query"), map[string]any{"name": "Card"}); r["found"] != true {
			t.Fatalf("session: %+v", r)
		}
	}
	sessions, _ := os.ReadDir(filepath.Join(data, "index", "sessions"))
	if len(sessions) != 1 {
		t.Fatalf("expected one private index, got %d", len(sessions))
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if sessions, _ := os.ReadDir(filepath.Join(data, "index", "sessions")); len(sessions) != 0 {
		t.Fatalf("private index not removed: %v", sessions)
	}
}

// Without index_on_start the first call builds the index and waits for it.
func TestIndexScopeLazyFirstBuild(t *testing.T) {
	root := canonicalTempDir(t)
	writeTree(t, root, payFiles)
	s, err := NewIndexScope(context.Background(), IndexScopeOptions{Root: root, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if gen := s.Engine().Status().Generation; gen != 0 {
		t.Fatalf("index built before first use: generation %d", gen)
	}
	r := call(t, context.Background(), toolFrom(t, s.Tools(), "graph_query"), map[string]any{"name": "Card"})
	if r["found"] != true {
		t.Fatalf("lazy build: %+v", r)
	}
}

// TestIndexScopeResolvedCallGraph checks the graph tools on resolved edges
// in a language without a type checker: a method with the same name on an
// unrelated class is not a caller, depth follows declarations, tests are
// found through the pack's test markers, and impact reports callers from
// other modules of a public method as a breaking change.
func TestIndexScopeResolvedCallGraph(t *testing.T) {
	root := canonicalTempDir(t)
	writeTree(t, root, map[string]string{
		"app/repo.py": `class Repo:
    def save(self, item):
        return item


class Cache:
    def save(self, item):
        return None
`,
		"app/service.py": `from app.repo import Repo, Cache


class Service:
    def __init__(self, repo: Repo, cache: Cache):
        self.repo = repo
        self.cache = cache

    def store(self, item):
        return self.repo.save(item)

    def warm(self, item):
        self.cache.save(item)
`,
		"api/views.py": `from app.service import Service


def create(svc: Service):
    return svc.store(1)
`,
		"tests/test_service.py": `from api.views import create


def test_create():
    assert create(None) == 1
`,
	})
	scope := newTestScope(t, root, false)
	ctx := context.Background()

	c := call(t, ctx, toolFrom(t, scope.Tools(), "find_callers"), map[string]any{"name": "Repo.save", "depth": 3})
	levels, _ := c["callers_by_depth"].([]map[string]map[string][]string)
	var got []string
	for d, level := range levels {
		for callee, byLabel := range level {
			for label, callers := range byLabel {
				for _, e := range callers {
					got = append(got, fmt.Sprintf("%d %s<-%s %s", d, callee, e, label))
				}
			}
		}
	}
	want := []string{
		"0 Repo.save<-Service.store (app/service.py:10) type_hinted",
		"1 Service.store<-create (api/views.py:5) type_hinted",
		"2 create<-test_create (tests/test_service.py:5) import_resolved",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("find_callers(Repo.save) = %q, want %q", got, want)
	}

	tm := call(t, ctx, toolFrom(t, scope.ImpactTools(), "test_map"), map[string]any{"symbol": "app/repo.py"})
	if tests, _ := tm["tests"].([]string); !reflect.DeepEqual(tests, []string{"test_create"}) {
		t.Fatalf("test_map(app/repo.py) = %+v", tm)
	}

	im := call(t, ctx, toolFrom(t, scope.ImpactTools(), "impact_analysis"), map[string]any{"file": "app/service.py", "start_line": 9, "end_line": 10})
	affected, _ := im["affected_symbols"].([]map[string]any)
	var store map[string]any
	for _, a := range affected { // the enclosing class overlaps the range too
		if a["symbol"] == "store" {
			store = a
		}
	}
	if store == nil || im["potential_breaking_change"] != true ||
		!reflect.DeepEqual(store["callers"], []string{"create"}) || !reflect.DeepEqual(store["tests"], []string{"test_create"}) {
		t.Fatalf("impact_analysis = %+v", im)
	}
}
