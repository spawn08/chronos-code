package indexer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/indexer/precise"
	"github.com/spawn08/chronos-code/internal/indexbench"
)

// Same scenarios and corpus as internal/graph/index_bench_test.go, so the two
// implementations are directly comparable. Run with `make bench-index`.
const (
	benchProbeDir = "internal/config"
	benchProbePkg = "config"
)

type indexBench struct {
	root, dir string
	e         *Engine
	probe     *indexbench.Probe
}

func newIndexBench(b *testing.B, build bool) *indexBench {
	b.Helper()
	src, err := filepath.Abs("..")
	if err != nil {
		b.Fatal(err)
	}
	root := indexbench.CopyRepo(b, src)
	ib := &indexBench{root: root, dir: filepath.Join(b.TempDir(), "index")}
	ib.probe = indexbench.NewProbe(b, root, benchProbeDir, benchProbePkg)
	e, err := Open(Options{Root: root, Dir: ib.dir})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ib.e.Close() })
	ib.e = e
	if build {
		if _, err := e.Reconcile(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
	return ib
}

func (ib *indexBench) update(b *testing.B) {
	b.Helper()
	st, err := ib.e.Update(context.Background(), []string{ib.probe.Path()})
	if err != nil {
		b.Fatal(err)
	}
	if st.Parsed != 1 {
		b.Fatalf("update parsed %d files", st.Parsed)
	}
}

func BenchmarkIndexFresh(b *testing.B) {
	ib := newIndexBench(b, false)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		e, err := Open(Options{Root: ib.root, Dir: filepath.Join(b.TempDir(), "index")})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := e.Reconcile(ctx); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = e.Close()
		b.StartTimer()
	}
}

func BenchmarkIndexNoop(b *testing.B) {
	ib := newIndexBench(b, true)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ib.e.Reconcile(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIndexReopen measures a process restart: open the index and
// reconcile with nothing changed.
func BenchmarkIndexReopen(b *testing.B) {
	ib := newIndexBench(b, true)
	ctx := context.Background()
	if err := ib.e.Close(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e, err := Open(Options{Root: ib.root, Dir: ib.dir})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := e.Reconcile(ctx); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = e.Close()
		b.StartTimer()
	}
	b.StopTimer()
	e, err := Open(Options{Root: ib.root, Dir: ib.dir})
	if err != nil {
		b.Fatal(err)
	}
	ib.e = e
}

func BenchmarkIndexEditBody(b *testing.B) {
	ib := newIndexBench(b, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.probe.Write(i+1, false, true)
		ib.update(b)
	}
}

// BenchmarkIndexEditBodyDuringPrecise is BenchmarkIndexEditBody while
// the type-checked tier loads the whole module again and again in the
// background (M4: edits stay < 50 ms while precise loads run).
func BenchmarkIndexEditBodyDuringPrecise(b *testing.B) {
	if err := precise.Available(); err != nil {
		b.Skip(err)
	}
	ib := newIndexBench(b, true)
	ctx, cancel := context.WithCancel(context.Background())
	loads := make(chan int, 1)
	go func() {
		n := 0
		defer func() { loads <- n }()
		for ctx.Err() == nil {
			n++
			_, _ = precise.Load(ctx, ib.root, ".", nil, nil)
		}
	}()
	time.Sleep(500 * time.Millisecond) // let the first load get going
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.probe.Write(i+1, false, true)
		ib.update(b)
	}
	b.StopTimer()
	cancel()
	b.ReportMetric(float64(<-loads), "loads_started")
}

// BenchmarkPreciseLoad is one type-checked load of this whole repository.
func BenchmarkPreciseLoad(b *testing.B) {
	if err := precise.Available(); err != nil {
		b.Skip(err)
	}
	ib := newIndexBench(b, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := precise.Load(context.Background(), ib.root, ".", nil, nil)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(res.Dirs)), "dirs")
	}
}

func BenchmarkIndexEditSignature(b *testing.B) {
	ib := newIndexBench(b, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.probe.Write(0, i%2 == 0, true)
		ib.update(b)
	}
}

func BenchmarkIndexEditAddDecl(b *testing.B) {
	ib := newIndexBench(b, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		ib.probe.Write(0, false, false)
		ib.update(b)
		b.StartTimer()
		ib.probe.Write(0, false, true)
		ib.update(b)
	}
}

func BenchmarkIndexEditRemoveDecl(b *testing.B) {
	ib := newIndexBench(b, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.probe.Write(0, false, false)
		ib.update(b)
		b.StopTimer()
		ib.probe.Write(0, false, true)
		ib.update(b)
		b.StartTimer()
	}
}

// BenchmarkIndexQueryAfterEdit: edit, update, then look the symbol up in a
// fresh snapshot — the full path to "edit visible to queries".
func BenchmarkIndexQueryAfterEdit(b *testing.B) {
	ib := newIndexBench(b, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.probe.Write(i+1, false, true)
		ib.update(b)
		sn := ib.e.Snapshot()
		if len(symbol(sn, "IndexBenchProbe")) != 1 {
			b.Fatal("probe symbol not visible")
		}
		sn.Release()
	}
}

// BenchmarkIndexOpenQuery is time-to-first-answer after a restart: open the
// stored index and answer a symbol lookup, before any reconcile.
func BenchmarkIndexOpenQuery(b *testing.B) {
	ib := newIndexBench(b, true)
	if err := ib.e.Close(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e, err := Open(Options{Root: ib.root, Dir: ib.dir})
		if err != nil {
			b.Fatal(err)
		}
		sn := e.Snapshot()
		if len(symbol(sn, "IndexBenchProbe")) != 1 {
			b.Fatal("probe symbol not found")
		}
		sn.Release()
		b.StopTimer()
		_ = e.Close()
		b.StartTimer()
	}
	b.StopTimer()
	e, err := Open(Options{Root: ib.root, Dir: ib.dir})
	if err != nil {
		b.Fatal(err)
	}
	ib.e = e
}
