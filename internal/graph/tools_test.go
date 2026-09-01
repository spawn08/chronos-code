package graph

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos/engine/tool"
)

func TestToolsAgainstOwnRepo(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "graph.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(context.Background()); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}

	defs := Tools(store, root)
	byName := make(map[string]*tool.Definition)
	for _, d := range defs {
		byName[d.Name] = d
	}
	for _, want := range []string{"graph_query", "find_callers", "find_implementations", "multi_resolution_view", "resolve_symbol", "codebase_search"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("missing tool %q", want)
		}
	}

	ctx := context.Background()

	out, err := byName["graph_query"].Handler(ctx, map[string]any{"name": "OpenStore", "kind": "func"})
	if err != nil {
		t.Fatalf("graph_query: %v", err)
	}
	res := out.(map[string]any)
	if res["found"] != true {
		t.Fatalf("graph_query: expected to find OpenStore, got %+v", res)
	}

	out, err = byName["find_callers"].Handler(ctx, map[string]any{"name": "migrate"})
	if err != nil {
		t.Fatalf("find_callers: %v", err)
	}
	t.Logf("find_callers(migrate) = %+v", out)

	out, err = byName["multi_resolution_view"].Handler(ctx, map[string]any{"target": "", "level": "L0"})
	if err != nil {
		t.Fatalf("multi_resolution_view L0: %v", err)
	}
	l0 := out.(map[string]any)
	if len(l0["packages"].([]string)) == 0 {
		t.Fatal("multi_resolution_view L0: expected at least one package")
	}

	out, err = byName["resolve_symbol"].Handler(ctx, map[string]any{"name": "OpenStore"})
	if err != nil {
		t.Fatalf("resolve_symbol: %v", err)
	}
	t.Logf("resolve_symbol(OpenStore) = %+v", out)
}

func TestCodebaseSearchTool(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	for _, symbol := range []Symbol{
		{Name: "Router", Kind: KindStruct, Package: "router", File: "router.go", Line: 1, EndLine: 2, Doc: "Routes coding tasks."},
		{Name: "SearchHelper", Kind: KindFunc, Package: "router", File: "search.go", Line: 1, EndLine: 2, Doc: "Uses the Router."},
	} {
		if err := store.InsertSymbol(ctx, symbol); err != nil {
			t.Fatalf("InsertSymbol(%s): %v", symbol.Name, err)
		}
	}

	var search *tool.Definition
	for _, def := range Tools(store, "") {
		if def.Name == "codebase_search" {
			search = def
			break
		}
	}
	if search == nil {
		t.Fatal("codebase_search is not registered")
	}

	out, err := search.Handler(ctx, map[string]any{"query": "Router", "top_k": 10})
	if err != nil {
		t.Fatalf("codebase_search: %v", err)
	}
	result := out.(map[string]any)
	symbols := result["symbols"].([]map[string]any)
	if len(symbols) != 2 {
		t.Fatalf("codebase_search symbols = %+v, want exact and FTS matches", symbols)
	}
	if symbols[0]["name"] != "Router" || symbols[1]["name"] != "SearchHelper" {
		t.Fatalf("codebase_search order = %+v, want exact Router before FTS match", symbols)
	}
	if _, err := search.Handler(ctx, map[string]any{"query": ""}); err == nil {
		t.Fatal("codebase_search accepted an empty query")
	}
}
