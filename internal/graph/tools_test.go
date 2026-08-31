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
	for _, want := range []string{"graph_query", "find_callers", "find_implementations", "multi_resolution_view", "resolve_symbol"} {
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
