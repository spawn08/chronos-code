package treesitter

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/indexer/extract/packs"
)

// BenchmarkParse measures full-parse throughput per pack over a corpus of
// real files: CHRONOS_TS_CORPUS=<dir> with one subdirectory per grammar name
// (dir/python/*.py, dir/c_sharp/*.cs, ...). Reports MB/s, the slowest file,
// files dropped by the deadline or a limit, and files kept as partial trees.
// See docs/chronos-indexer.md, "M6 parser runtime spike".
//
//	CHRONOS_TS_CORPUS=/tmp/corpus go test -tags "$(make -s grammar-tags)" \
//	  ./indexer/extract/treesitter -run '^$' -bench Parse -benchtime 3x
func BenchmarkParse(b *testing.B) {
	dir := os.Getenv("CHRONOS_TS_CORPUS")
	if dir == "" {
		b.Skip("CHRONOS_TS_CORPUS not set")
	}
	reg, err := packs.Load()
	if err != nil {
		b.Fatal(err)
	}
	for _, p := range reg.Packs() {
		srcs, total := readCorpus(b, filepath.Join(dir, p.Grammar))
		if len(srcs) == 0 {
			continue
		}
		b.Run(p.ID, func(b *testing.B) {
			rt := New(Options{})
			if _, err := rt.Language(p.Grammar); err != nil {
				b.Skip(err)
			}
			b.SetBytes(int64(total))
			var worst time.Duration
			dropped, partial := 0, 0
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dropped, partial = 0, 0
				for _, src := range srcs {
					t0 := time.Now()
					tree, err := rt.Parse(p.Grammar, src)
					worst = max(worst, time.Since(t0))
					if err != nil {
						dropped++
						continue
					}
					if tree.Partial {
						partial++
					}
					tree.Release()
				}
			}
			b.ReportMetric(float64(worst.Microseconds())/1000, "worst-ms")
			b.ReportMetric(float64(dropped), "dropped-files")
			b.ReportMetric(float64(partial), "partial-files")
			b.ReportMetric(float64(len(srcs)), "files")
		})
	}
}

func readCorpus(b *testing.B, dir string) ([][]byte, int) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0
	}
	var srcs [][]byte
	total := 0
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			b.Fatal(err)
		}
		srcs = append(srcs, data)
		total += len(data)
	}
	return srcs, total
}
