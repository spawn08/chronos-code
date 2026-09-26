package graph

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/indexbench"
)

// Query-path benchmarks for the graph tools over this repository's own
// index (IndexScope). The index is built once per benchmark binary; only
// query latency is timed:
//
//	go test ./internal/graph -run '^$' -bench '^BenchmarkScope' -benchmem
//
// BenchmarkScopeQueryAfterEdit edits a probe in internal/config, which about
// 40 packages import, so API changes there exercise reverse-importer work.
const (
	benchProbeDir = "internal/config"
	benchProbePkg = "config"
)

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
	benchTool(b, s.Tools(), "graph_query", map[string]any{"name": "Reconcile"})
}

func BenchmarkScopeGraphQueryMiss(b *testing.B) {
	s, _ := benchScope(b)
	benchTool(b, s.Tools(), "graph_query", map[string]any{"name": "Reconcle"})
}

func BenchmarkScopeGraphQueryBatch(b *testing.B) {
	s, _ := benchScope(b)
	benchTool(b, s.Tools(), "graph_query", map[string]any{"names": []any{"Reconcile", "Update", "Snapshot", "Publish"}})
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
		"file": filepath.Join(root, "indexer/engine.go"), "start_line": 1, "end_line": 700,
	})
}

func BenchmarkScopeCodebaseContext(b *testing.B) {
	s, _ := benchScope(b)
	benchTool(b, s.Tools(), "codebase_context", map[string]any{"query": "Reconcile", "max_tokens": 4096})
}

func BenchmarkScopeCodebaseSearch(b *testing.B) {
	s, _ := benchScope(b)
	benchTool(b, s.Tools(), "codebase_search", map[string]any{"query": "graph index store", "top_k": 10})
}

// BenchmarkScopeQueryAfterEdit is an edit made visible (as the watcher does
// on its event) followed by the first graph tool call.
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

// Federation benchmarks (M9): this repository federating ../chronos, whose
// Go module it imports. Skipped when ../chronos is absent.
var fedScope struct {
	once  sync.Once
	scope *IndexScope
	err   error
}

func benchFederatedScope(b *testing.B) *IndexScope {
	b.Helper()
	fedScope.once.Do(func() {
		root, err := filepath.Abs("../..")
		if err == nil {
			root, err = filepath.EvalSymlinks(root)
		}
		if err != nil {
			fedScope.err = err
			return
		}
		chronos := filepath.Join(filepath.Dir(root), "chronos")
		if _, err := os.Stat(filepath.Join(chronos, "go.mod")); err != nil {
			fedScope.err = err
			return
		}
		dir, err := os.MkdirTemp("", "scope-bench-fed-*")
		if err != nil {
			fedScope.err = err
			return
		}
		scope, err := NewIndexScope(context.Background(), IndexScopeOptions{
			Root: root, DataDir: dir, IndexOnStart: true, Federation: []FederatedRoot{{Root: chronos}},
		})
		if err != nil {
			fedScope.err = err
			return
		}
		<-scope.Engine().Ready()
		fedScope.scope = scope
	})
	if fedScope.err != nil {
		b.Skipf("federated scope: %v", fedScope.err)
	}
	return fedScope.scope
}

func BenchmarkScopeFederatedGraphQuery(b *testing.B) {
	benchTool(b, benchFederatedScope(b).Tools(), "graph_query", map[string]any{"name": "Reconcile"})
}

// Callers of chronos's tool registry constructor, most of them in this
// repository (cross-repository import edges).
func BenchmarkScopeFederatedFindCallers(b *testing.B) {
	benchTool(b, benchFederatedScope(b).Tools(), "find_callers", map[string]any{"name": "NewRegistry", "depth": 2})
}

func BenchmarkScopeFederatedFindCallersDepth3(b *testing.B) {
	benchTool(b, benchFederatedScope(b).Tools(), "find_callers", map[string]any{"name": "Execute", "depth": 3})
}

func BenchmarkScopeFederatedCodebaseSearch(b *testing.B) {
	benchTool(b, benchFederatedScope(b).Tools(), "codebase_search", map[string]any{"query": "graph index store", "top_k": 10})
}
