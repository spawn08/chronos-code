package graph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

// The same workspace answered by the type-checked SQLite graph and by the
// syntactic chronos index gives the same relationships through the
// unchanged tool contracts. The one intended difference: the index also
// returns interface method specs as declarations.
func TestIndexScopeMatchesStoreTools(t *testing.T) {
	root := canonicalTempDir(t)
	writeTree(t, root, payFiles)
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := NewIndexer(store, root).IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	scope := newTestScope(t, root, false)
	oldTools := append(Tools(store, root), ImpactTools(store, root)...)
	newTools := append(scope.Tools(), scope.ImpactTools()...)
	both := func(name string, args map[string]any) (map[string]any, map[string]any) {
		return call(t, ctx, toolFrom(t, oldTools, name), args), call(t, ctx, toolFrom(t, newTools, name), args)
	}

	type decl struct{ Name, Kind, Package, Receiver string }
	decls := func(v any) []decl {
		var out []decl
		for _, s := range v.([]map[string]any) {
			out = append(out, decl{s["name"].(string), s["kind"].(string), s["package"].(string), s["receiver"].(string)})
		}
		return out
	}
	oldQ, newQ := both("graph_query", map[string]any{"name": "Pay"})
	oldDecls, newDecls := decls(oldQ["symbols"]), decls(newQ["symbols"])
	for _, d := range oldDecls {
		if !slices.Contains(newDecls, d) {
			t.Fatalf("graph_query(Pay): index lost %+v; got %+v", d, newDecls)
		}
	}
	for _, d := range newDecls {
		if !slices.Contains(oldDecls, d) && d.Receiver != "Payer" {
			t.Fatalf("graph_query(Pay): unexpected extra declaration %+v", d)
		}
	}

	for _, name := range []string{"record", "Pay", "Handle"} {
		oldC, newC := both("find_callers", map[string]any{"name": name, "depth": 3})
		if !reflect.DeepEqual(oldC["callers_by_depth"], newC["callers_by_depth"]) {
			t.Fatalf("find_callers(%s): store %v, index %v", name, oldC["callers_by_depth"], newC["callers_by_depth"])
		}
	}
	oldI, newI := both("find_implementations", map[string]any{"name": "Payer"})
	if !reflect.DeepEqual(oldI["implementations"], newI["implementations"]) {
		t.Fatalf("find_implementations: store %v, index %v", oldI["implementations"], newI["implementations"])
	}
	oldT, newT := both("test_map", map[string]any{"symbol": "Pay"})
	if !reflect.DeepEqual(oldT["tests"], newT["tests"]) {
		t.Fatalf("test_map: store %v, index %v", oldT["tests"], newT["tests"])
	}
	impactArgs := map[string]any{"file": filepath.Join(root, "service.go"), "start_line": 9, "end_line": 9}
	oldA, newA := both("impact_analysis", impactArgs)
	if !reflect.DeepEqual(oldA["affected_symbols"], newA["affected_symbols"]) || oldA["potential_breaking_change"] != newA["potential_breaking_change"] {
		t.Fatalf("impact_analysis: store %v, index %v", oldA, newA)
	}
	relA := call(t, ctx, toolFrom(t, newTools, "impact_analysis"), map[string]any{"file": "service.go", "start_line": 9, "end_line": 9})
	if !reflect.DeepEqual(relA["affected_symbols"], newA["affected_symbols"]) {
		t.Fatalf("impact_analysis must accept relative paths: %v", relA)
	}

	contextArgs := map[string]any{"query": "record", "max_tokens": 4096}
	oldE, err := toolFrom(t, oldTools, "codebase_context").Handler(ctx, contextArgs)
	if err != nil {
		t.Fatal(err)
	}
	newE, err := toolFrom(t, newTools, "codebase_context").Handler(ctx, contextArgs)
	if err != nil {
		t.Fatal(err)
	}
	roles := func(r *evidenceResult) []string {
		var out []string
		for _, it := range r.Items {
			out = append(out, it.Role+":"+it.Name)
		}
		return out
	}
	oldR, newR := oldE.(*evidenceResult), newE.(*evidenceResult)
	if !slices.Equal(roles(oldR), roles(newR)) {
		t.Fatalf("codebase_context roles: store %v, index %v", roles(oldR), roles(newR))
	}
	for _, it := range newR.Items {
		if it.Source == nil || it.Source.Freshness != "verified" {
			t.Fatalf("codebase_context over the index must verify excerpts: %+v", it)
		}
	}

	oldS, newS := both("codebase_search", map[string]any{"query": "record"})
	first := func(m map[string]any) string { return m["symbols"].([]map[string]any)[0]["name"].(string) }
	if first(oldS) != first(newS) {
		t.Fatalf("codebase_search first hit: store %s, index %s", first(oldS), first(newS))
	}
	oldL1, newL1 := both("multi_resolution_view", map[string]any{"level": "L1", "target": "pay"})
	names := func(m map[string]any) []string {
		var out []string
		for _, s := range m["symbols"].([]map[string]any) {
			if s["receiver"] != "Payer" {
				out = append(out, s["name"].(string))
			}
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(names(oldL1), names(newL1)) {
		t.Fatalf("multi_resolution_view L1: store %v, index %v", names(oldL1), names(newL1))
	}
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
	c := call(t, ctx, toolFrom(t, scope.Tools(), "find_callers"), map[string]any{"name": "Handle"})
	if c["resolution"] != "name_matched" || c["note"] != nil {
		t.Fatalf("non-empty callers must be labelled without a note: %+v", c)
	}
	c = call(t, ctx, toolFrom(t, scope.Tools(), "find_callers"), map[string]any{"name": "TestHandle"})
	if note, _ := c["note"].(string); c["resolution"] != "name_matched" || !strings.Contains(note, `No callers of "TestHandle"`) {
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
	// The SQLite store does not report freshness: its output is unchanged.
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	plain := call(t, ctx, toolFrom(t, Tools(store, root), "graph_query"), map[string]any{"name": "Missing"})
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
