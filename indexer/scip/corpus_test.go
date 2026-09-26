package scip

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cespare/xxhash/v2"
)

// TestCorpusFreshIndexes imports the SCIP edge baseline's indexes
// (benchmark/edges/run.sh) against their checkouts: every document of a
// fresh index must pass the check for indexes built elsewhere. Set
// CHRONOS_SCIP_CORPUS=1 (and CHRONOS_EDGES_CACHE if not the default).
func TestCorpusFreshIndexes(t *testing.T) {
	if os.Getenv("CHRONOS_SCIP_CORPUS") == "" {
		t.Skip("set CHRONOS_SCIP_CORPUS=1 to import the SCIP edge corpus")
	}
	cache := os.Getenv("CHRONOS_EDGES_CACHE")
	if cache == "" {
		home, _ := os.UserHomeDir()
		cache = filepath.Join(home, ".cache", "chronos-edges")
	}
	idx, _ := filepath.Glob(filepath.Join(cache, "*.scip"))
	if len(idx) == 0 {
		t.Skip("no SCIP indexes in " + cache)
	}
	for _, index := range idx {
		name := strings.TrimSuffix(filepath.Base(index), ".scip")
		root := filepath.Join(cache, name)
		t.Run(name, func(t *testing.T) {
			res, err := Import(context.Background(), Options{
				Root: root,
				Indexed: func(p string) (uint64, bool) {
					data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
					return xxhash.Sum64(data), err == nil
				},
				Skip: func(p string) bool { return strings.HasSuffix(p, ".go") },
			}, []Source{{Index: index}})
			if err != nil {
				t.Fatal(err)
			}
			reasons := make([]string, 0, len(res.Rejected))
			for p, why := range res.Rejected {
				reasons = append(reasons, p+": "+why)
			}
			sort.Strings(reasons)
			t.Logf("%d documents, %d sites, %d skipped, %d rejected", res.Docs, res.Sites, res.Skipped, len(res.Rejected))
			for _, r := range reasons {
				t.Errorf("rejected %s", r)
			}
		})
	}
}

// TestCorpusStaleDetection inserts one blank line into each document's
// file (at half or nine tenths of its length), backdates the file so the
// modification-time check passes, and counts how many stale documents the
// occurrence check still rejects.
func TestCorpusStaleDetection(t *testing.T) {
	if os.Getenv("CHRONOS_SCIP_CORPUS") == "" {
		t.Skip("set CHRONOS_SCIP_CORPUS=1 to import the SCIP edge corpus")
	}
	cache := os.Getenv("CHRONOS_EDGES_CACHE")
	if cache == "" {
		home, _ := os.UserHomeDir()
		cache = filepath.Join(home, ".cache", "chronos-edges")
	}
	idx, _ := filepath.Glob(filepath.Join(cache, "*.scip"))
	for _, index := range idx {
		name := strings.TrimSuffix(filepath.Base(index), ".scip")
		src := filepath.Join(cache, name)
		st, err := os.Stat(index)
		if err != nil {
			t.Fatal(err)
		}
		old := st.ModTime().Add(-time.Hour)
		for _, frac := range []int{50, 90} {
			root := t.TempDir()
			var docs []string
			inserted := map[string]int{} // path -> 1-based line of the blank line
			_ = ReadFile(index, nil, func(d Document) error {
				if strings.HasSuffix(d.Path, ".go") {
					return nil
				}
				data, err := os.ReadFile(filepath.Join(src, d.Path))
				if err != nil {
					return nil
				}
				lines := strings.SplitAfter(string(data), "\n")
				at := len(lines) * frac / 100
				edited := strings.Join(lines[:at], "") + "\n" + strings.Join(lines[at:], "")
				dst := filepath.Join(root, d.Path)
				_ = os.MkdirAll(filepath.Dir(dst), 0o755)
				_ = os.WriteFile(dst, []byte(edited), 0o644)
				_ = os.Chtimes(dst, old, old)
				docs = append(docs, d.Path)
				inserted[d.Path] = at + 1
				return nil
			})
			res, err := Import(context.Background(), Options{
				Root: root,
				Indexed: func(p string) (uint64, bool) {
					data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
					return xxhash.Sum64(data), err == nil
				},
			}, []Source{{Index: index, Dir: "."}})
			if err != nil {
				t.Fatal(err)
			}
			// Sites below the blank line are at the wrong position: facts
			// a query could misuse.
			wrong, total := 0, 0
			for _, d := range res.Dirs {
				for p, f := range d.Files {
					for _, c := range append(f.Calls, f.TypeRefs...) {
						total++
						if int(c.Line) >= inserted[p] {
							wrong++
						}
					}
				}
			}
			if len(docs) > 0 {
				t.Logf("%s, line inserted at %d%%: %d of %d stale documents rejected, %d accepted with %d sites, %d of them misplaced",
					name, frac, len(res.Rejected), len(docs), res.Docs, total, wrong)
			}
		}
	}
}
