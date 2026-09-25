package graph

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/indexbench"
)

// Query-path benchmarks for the chronos index (IndexScope), with the same
// tools and arguments as the Query benchmarks for the SQLite store, so the
// two can be compared directly:
//
//	go test ./internal/graph -run '^$' -bench '^BenchmarkScope' -benchmem
var ownRepoScope struct {
	once  sync.Once
	scope *IndexScope
	root  string
	err   error
}

func benchScope(b *testing.B) (*IndexScope, string) {
	b.Helper()
	ownRepoScope.once.Do(func() {
		root, err := filepath.Abs("../..")
		if err == nil {
			root, err = filepath.EvalSymlinks(root)
		}
		if err != nil {
			ownRepoScope.err = err
			return
		}
		dir, err := os.MkdirTemp("", "scope-bench-*")
		if err != nil {
			ownRepoScope.err = err
			return
		}
		scope, err := NewIndexScope(context.Background(), IndexScopeOptions{Root: root, DataDir: dir, IndexOnStart: true})
		if err != nil {
			ownRepoScope.err = err
			return
		}
		<-scope.Engine().Ready()
		ownRepoScope.scope, ownRepoScope.root = scope, root
	})
	if ownRepoScope.err != nil {
		b.Fatalf("index own repo: %v", ownRepoScope.err)
	}
	return ownRepoScope.scope, ownRepoScope.root
}

func BenchmarkScopeGraphQuery(b *testing.B) {
	s, _ := benchScope(b)
	benchTool(b, s.Tools(), "graph_query", map[string]any{"name": "IndexAll"})
}

func BenchmarkScopeGraphQueryMiss(b *testing.B) {
	s, _ := benchScope(b)
	benchTool(b, s.Tools(), "graph_query", map[string]any{"name": "IndxAl"})
}

func BenchmarkScopeGraphQueryBatch(b *testing.B) {
	s, _ := benchScope(b)
	benchTool(b, s.Tools(), "graph_query", map[string]any{"names": []any{"IndexAll", "Reconcile", "Update", "Snapshot"}})
}

func BenchmarkScopeFindCallersDepth3(b *testing.B) {
	s, _ := benchScope(b)
	benchTool(b, s.Tools(), "find_callers", map[string]any{"name": "Execute", "depth": 3})
}

func BenchmarkScopeFindImplementations(b *testing.B) {
	s, _ := benchScope(b)
	benchTool(b, s.Tools(), "find_implementations", map[string]any{"name": "Backend"})
}

func BenchmarkScopeImpact(b *testing.B) {
	s, root := benchScope(b)
	benchTool(b, s.ImpactTools(), "impact_analysis", map[string]any{
		"file": filepath.Join(root, "internal/graph/indexer.go"), "start_line": 1, "end_line": 700,
	})
}

func BenchmarkScopeCodebaseContext(b *testing.B) {
	s, _ := benchScope(b)
	benchTool(b, s.Tools(), "codebase_context", map[string]any{"query": "IndexAll", "max_tokens": 4096})
}

func BenchmarkScopeCodebaseSearch(b *testing.B) {
	s, _ := benchScope(b)
	benchTool(b, s.Tools(), "codebase_search", map[string]any{"query": "graph index store", "top_k": 10})
}

// BenchmarkScopeQueryAfterEdit is an edit made visible (as the watcher does
// on its event) followed by the first graph tool call: the M2 counterpart of
// BenchmarkIndexQueryAfterEdit for the SQLite graph.
func BenchmarkScopeQueryAfterEdit(b *testing.B) {
	src, err := filepath.Abs("../..")
	if err != nil {
		b.Fatal(err)
	}
	root := indexbench.CopyRepo(b, src)
	probe := indexbench.NewProbe(b, root, benchProbeDir, benchProbePkg)
	scope, err := NewIndexScope(context.Background(), IndexScopeOptions{Root: root, DataDir: b.TempDir(), IndexOnStart: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = scope.Close() })
	<-scope.Engine().Ready()
	var query = scope.Tools()[0]
	ctx := context.Background()
	args := map[string]any{"name": "IndexBenchProbe"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		probe.Write(i+1, false, true)
		if _, err := scope.Engine().Update(ctx, []string{probe.Path()}); err != nil {
			b.Fatal(err)
		}
		if _, err := query.Handler(ctx, args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScopePrefetch(b *testing.B) {
	s, _ := benchScope(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A fresh session each time: nothing is subtracted as already seen.
		if _, err := s.Prefetch(storage.WithSession(ctx, strconv.Itoa(i)), "the watcher should flush a burst of file events into one Engine.Update", 1500); err != nil {
			b.Fatal(err)
		}
	}
}
