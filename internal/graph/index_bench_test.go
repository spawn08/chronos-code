package graph

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos-code/internal/indexbench"
	"github.com/spawn08/chronos/engine/tool"
)

// Indexing benchmarks over a private copy of this repository, so edits never
// touch the checkout. Scenario names match the chronos indexer benchmarks
// (docs/chronos-indexer.md, "Measurement"). Each iteration is expensive; run
// with a fixed iteration count:
//
//	make bench-index
//
// The probe lives in internal/config, which about 40 packages import, so API
// changes there exercise reverse-importer reloads.
const (
	benchProbeDir = "internal/config"
	benchProbePkg = "config"
)

type indexBench struct {
	root  string
	store *Store
	ix    *Indexer
	probe *indexbench.Probe
}

func newIndexBench(b *testing.B, build bool) *indexBench {
	b.Helper()
	src, err := filepath.Abs("../..")
	if err != nil {
		b.Fatal(err)
	}
	root := indexbench.CopyRepo(b, src)
	probe := indexbench.NewProbe(b, root, benchProbeDir, benchProbePkg)
	store, err := OpenStore(filepath.Join(b.TempDir(), "graph.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	ib := &indexBench{root: root, store: store, ix: NewIndexer(store, root), probe: probe}
	if build {
		ib.index(b)
	}
	return ib
}

func (ib *indexBench) index(b *testing.B) {
	b.Helper()
	if _, err := ib.ix.IndexAll(context.Background()); err != nil {
		b.Fatalf("IndexAll: %v", err)
	}
}

func BenchmarkIndexFresh(b *testing.B) {
	ib := newIndexBench(b, false)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		store, err := OpenStore(filepath.Join(b.TempDir(), "graph.db"))
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := NewIndexer(store, ib.root).IndexAll(ctx); err != nil {
			b.Fatalf("IndexAll: %v", err)
		}
		b.StopTimer()
		_ = store.Close()
		b.StartTimer()
	}
}

// BenchmarkIndexNoop is a reconciliation with nothing changed, same process.
func BenchmarkIndexNoop(b *testing.B) {
	ib := newIndexBench(b, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.index(b)
	}
}

// BenchmarkIndexReopen is a reconciliation with nothing changed by a new
// indexer on an existing store (process restart: cold scan cache).
func BenchmarkIndexReopen(b *testing.B) {
	ib := newIndexBench(b, true)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NewIndexer(ib.store, ib.root).IndexAll(ctx); err != nil {
			b.Fatalf("IndexAll: %v", err)
		}
	}
}

func BenchmarkIndexEditBody(b *testing.B) {
	ib := newIndexBench(b, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.probe.Write(i+1, false, true)
		ib.index(b)
	}
}

func BenchmarkIndexEditSignature(b *testing.B) {
	ib := newIndexBench(b, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.probe.Write(0, i%2 == 0, true)
		ib.index(b)
	}
}

func BenchmarkIndexEditAddDecl(b *testing.B) {
	ib := newIndexBench(b, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		ib.probe.Write(0, false, false)
		ib.index(b)
		b.StartTimer()
		ib.probe.Write(0, false, true)
		ib.index(b)
	}
}

func BenchmarkIndexEditRemoveDecl(b *testing.B) {
	ib := newIndexBench(b, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.probe.Write(0, false, false)
		ib.index(b)
		b.StopTimer()
		ib.probe.Write(0, false, true)
		ib.index(b)
		b.StartTimer()
	}
}

// BenchmarkIndexQueryAfterEdit is the latency an agent pays on the first graph
// tool call after a body edit, before any watcher has reconciled it.
func BenchmarkIndexQueryAfterEdit(b *testing.B) {
	ib := newIndexBench(b, true)
	scope := NewRequestScopeForIndexer(ib.ix)
	b.Cleanup(func() { _ = scope.Close() })
	var def *tool.Definition
	for _, d := range scope.Tools() {
		if d.Name == "graph_query" {
			def = d
		}
	}
	if def == nil {
		b.Fatal("graph_query tool not found")
	}
	ctx := context.Background()
	args := map[string]any{"name": "IndexBenchProbe"}
	if _, err := def.Handler(ctx, args); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.probe.Write(i+1, false, true)
		v, err := def.Handler(ctx, args)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := json.Marshal(v); err != nil {
			b.Fatal(err)
		}
	}
}
