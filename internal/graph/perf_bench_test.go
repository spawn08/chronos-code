package graph

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/spawn08/chronos/engine/tool"
)

// Query-path benchmarks over this repository's own graph. The index is built
// once per benchmark binary into a temporary database; only query latency is
// timed. Run with:
//
//	go test ./internal/graph -run '^$' -bench 'Query|Scan' -benchmem
var ownRepoGraph struct {
	once  sync.Once
	store *Store
	root  string
	err   error
}

func ownRepoStore(b *testing.B) (*Store, string) {
	b.Helper()
	ownRepoGraph.once.Do(func() {
		root, err := filepath.Abs("../..")
		if err != nil {
			ownRepoGraph.err = err
			return
		}
		if canonical, err := filepath.EvalSymlinks(root); err == nil {
			root = canonical
		}
		dir, err := os.MkdirTemp("", "graph-bench-*")
		if err != nil {
			ownRepoGraph.err = err
			return
		}
		store, err := OpenStore(filepath.Join(dir, "graph.db"))
		if err != nil {
			ownRepoGraph.err = err
			return
		}
		if _, err := NewIndexer(store, root).IndexAll(context.Background()); err != nil {
			ownRepoGraph.err = err
			return
		}
		ownRepoGraph.store, ownRepoGraph.root = store, root
	})
	if ownRepoGraph.err != nil {
		b.Fatalf("index own repo: %v", ownRepoGraph.err)
	}
	return ownRepoGraph.store, ownRepoGraph.root
}

func benchTool(b *testing.B, defs []*tool.Definition, name string, args map[string]any) {
	b.Helper()
	var def *tool.Definition
	for _, d := range defs {
		if d.Name == name {
			def = d
		}
	}
	if def == nil {
		b.Fatalf("tool %s not found", name)
	}
	ctx := context.Background()
	if _, err := def.Handler(ctx, args); err != nil {
		b.Fatalf("%s: %v", name, err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, err := def.Handler(ctx, args)
		if err != nil {
			b.Fatalf("%s: %v", name, err)
		}
		if _, err := json.Marshal(v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScanOwnRepo(b *testing.B) {
	_, root := ownRepoStore(b)
	ix := NewIndexer(nil, root)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ix.scan(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryCallersOf(b *testing.B) {
	store, _ := ownRepoStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.CallersOf(ctx, "Execute"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryGraphQuery(b *testing.B) {
	store, root := ownRepoStore(b)
	benchTool(b, Tools(store, root), "graph_query", map[string]any{"name": "IndexAll"})
}

func BenchmarkQueryGraphQueryMiss(b *testing.B) {
	store, root := ownRepoStore(b)
	benchTool(b, Tools(store, root), "graph_query", map[string]any{"name": "IndxAl"})
}

func BenchmarkQueryRequestScopeGraphQuery(b *testing.B) {
	store, root := ownRepoStore(b)
	scope := NewRequestScope(store, root)
	b.Cleanup(func() { _ = scope.Close() })
	benchTool(b, scope.Tools(), "graph_query", map[string]any{"name": "IndexAll"})
}

func BenchmarkQueryFindCallersDepth3(b *testing.B) {
	store, root := ownRepoStore(b)
	benchTool(b, Tools(store, root), "find_callers", map[string]any{"name": "Execute", "depth": 3})
}

func BenchmarkQueryImpact(b *testing.B) {
	store, root := ownRepoStore(b)
	benchTool(b, ImpactTools(store, root), "impact_analysis", map[string]any{
		"file": filepath.Join(root, "internal/graph/indexer.go"), "start_line": 1, "end_line": 700,
	})
}

func BenchmarkQueryCodebaseContext(b *testing.B) {
	store, root := ownRepoStore(b)
	benchTool(b, Tools(store, root), "codebase_context", map[string]any{"query": "IndexAll", "max_tokens": 4096})
}

func BenchmarkQueryCodebaseSearch(b *testing.B) {
	store, root := ownRepoStore(b)
	benchTool(b, Tools(store, root), "codebase_search", map[string]any{"query": "graph index store", "top_k": 10})
}
