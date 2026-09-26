package query_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/indexer/facts"
	"github.com/spawn08/chronos-code/indexer/query"
	"github.com/spawn08/chronos-code/indexer/store"
)

// BenchmarkScale builds a synthetic index of CHRONOS_SCALE_FILES files
// (default 100,000; the M5 target is 1,000,000) by streaming facts into the
// store — no source files are written — and reports the scale targets of
// docs/chronos-indexer.md. Each file declares 4 symbols and makes 6 calls.
//
//	CHRONOS_SCALE_FILES=1000000 go test ./indexer/query -run '^$' -bench Scale -benchtime=1x -timeout 60m
func BenchmarkScale(b *testing.B) {
	n := 100000
	if v, err := strconv.Atoi(os.Getenv("CHRONOS_SCALE_FILES")); err == nil && v > 0 {
		n = v
	}
	dir := b.TempDir()
	fileAt := func(i int, gen uint64) *facts.File {
		rng := rand.New(rand.NewPCG(uint64(i), gen))
		f := &facts.File{
			Path: fmt.Sprintf("svc%03d/pkg%03d/file%07d.go", i/10000, (i/100)%100, i), Lang: "go",
			Package: fmt.Sprintf("example.com/mono/svc%03d/pkg%03d", i/10000, (i/100)%100), Hash: uint64(i) + gen<<40,
			Imports: []facts.Import{{Path: "context", Line: 3}, {Path: fmt.Sprintf("example.com/mono/svc%03d/pkg%03d", rng.IntN(max(1, n/10000)), rng.IntN(100)), Line: 4}},
		}
		for k := 0; k < 4; k++ {
			name := fmt.Sprintf("Handle%dStep%d", i, k)
			f.Symbols = append(f.Symbols, facts.Symbol{Name: name, Kind: facts.KindFunc, Signature: "func " + name + "(ctx context.Context) error", Line: 10 + 10*k, EndLine: 18 + 10*k, Exported: true})
		}
		for c := 0; c < 6; c++ {
			f.Refs = append(f.Refs, facts.Ref{Enclosing: c % 4, Name: fmt.Sprintf("Handle%dStep%d", rng.IntN(n), rng.IntN(4)), Line: 12 + c})
		}
		return f
	}

	start := time.Now()
	s, err := store.Open(dir, "/mono", "bench")
	if err != nil {
		b.Fatal(err)
	}
	w := s.NewBase()
	for i := 0; i < n; i++ {
		if err := w.Add(fileAt(i, 0)); err != nil {
			b.Fatal(err)
		}
	}
	if err := w.Commit(); err != nil {
		b.Fatal(err)
	}
	build := time.Since(start)
	s.Close()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start = time.Now()
	s, err = store.Open(dir, "/mono", "bench")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	cache := query.NewCache()
	sn := s.Snapshot()
	if got := query.NewView(sn, cache).Symbols("Handle7Step1", ""); len(got) != 1 {
		b.Fatalf("lookup after reopen: %d", len(got))
	}
	sn.Release()
	reopen := time.Since(start)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	timeIt := func(iters int, fn func(i int)) time.Duration {
		t := time.Now()
		for i := 0; i < iters; i++ {
			fn(i)
		}
		return time.Since(t) / time.Duration(iters)
	}
	rng := rand.New(rand.NewPCG(9, 9))
	sn = s.Snapshot()
	v := query.NewView(sn, cache)
	lookup := timeIt(1000, func(int) { v.Symbols(fmt.Sprintf("Handle%dStep%d", rng.IntN(n), rng.IntN(4)), "") })
	callers := timeIt(1000, func(int) { v.Callers([]string{fmt.Sprintf("Handle%dStep%d", rng.IntN(n), rng.IntN(4))}) })
	// A task-like query: one identifier plus words found in every file.
	search := timeIt(20, func(int) { v.Search(fmt.Sprintf("Handle%dStep1 context error", rng.IntN(n)), 10) })
	if hits := v.Search("Handle7Step1 context error", 10); len(hits) == 0 || hits[0].Name != "Handle7Step1" {
		b.Fatalf("search at scale: %+v", hits)
	}
	sn.Release()

	var work int
	edit := timeIt(20, func(i int) {
		if err := s.Publish([]*facts.File{fileAt(rng.IntN(n), uint64(i+1))}, false); err != nil {
			b.Fatal(err)
		}
		sn := s.Snapshot()
		work = sn.BuildWork()
		sn.Release()
	})
	compact := timeIt(1, func(int) {
		if err := s.Compact(); err != nil {
			b.Fatal(err)
		}
	})
	man := s.Manifest()
	b.ReportMetric(float64(n), "files")
	b.ReportMetric(float64(len(man.Segments)), "shards")
	b.ReportMetric(build.Seconds(), "build_s")
	b.ReportMetric(float64(reopen.Microseconds())/1000, "reopen+lookup_ms")
	b.ReportMetric(float64(after.HeapInuse)/(1<<20), "heap_after_open_MiB")
	b.ReportMetric(float64(lookup.Microseconds())/1000, "lookup_ms")
	b.ReportMetric(float64(callers.Microseconds())/1000, "callers_ms")
	b.ReportMetric(float64(search.Microseconds())/1000, "search_ms")
	b.ReportMetric(float64(edit.Microseconds())/1000, "edit_publish_ms")
	b.ReportMetric(float64(work), "edit_route_entries")
	b.ReportMetric(float64(compact.Milliseconds()), "compact_ms")
	_ = before
}
