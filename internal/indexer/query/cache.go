package query

import (
	"slices"
	"strings"
	"sync"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/store"
)

// Cache holds derived, read-only results shared by the views of one engine.
// Search indexes live in the segments (persisted), so the only cached state
// is the package list of the latest generation, built on first use.
type Cache struct {
	mu   sync.Mutex
	pkgs *pkgList
}

type pkgList struct {
	gen   uint64
	names []string
}

// NewCache returns an empty cache.
func NewCache() *Cache { return &Cache{} }

// packageNames returns the sorted packages with at least one live file. It
// scans each segment's package-ordered file list once per generation.
func (c *Cache) packageNames(sn *store.Snapshot) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pkgs != nil && c.pkgs.gen == sn.Generation() {
		return c.pkgs.names
	}
	seen := map[string]bool{}
	for i := 0; i < sn.NumSegments(); i++ {
		seg := sn.Segment(i)
		prev, live := "", false
		for k := 0; k <= seg.NumPackageFiles(); k++ {
			var pkg string
			f := -1
			if k < seg.NumPackageFiles() {
				f = seg.PackageFile(k)
				pkg = seg.FilePackage(f)
			}
			if k == seg.NumPackageFiles() || (k > 0 && pkg != prev) {
				if live && !seen[prev] {
					seen[strings.Clone(prev)] = true
				}
				live = false
			}
			if f >= 0 {
				prev = pkg
				live = live || sn.Live(i, f)
			}
		}
	}
	names := make([]string, 0, len(seen))
	for p := range seen {
		names = append(names, p)
	}
	slices.Sort(names)
	c.pkgs = &pkgList{gen: sn.Generation(), names: names}
	return names
}

// liveCounts returns live declaration and reference counts: segment totals
// minus the records of dead files, so its cost is O(segments + overlay files).
func liveCounts(sn *store.Snapshot) (decls, refs int) {
	for i := 0; i < sn.NumSegments(); i++ {
		seg := sn.Segment(i)
		decls += seg.NumDecls()
		refs += seg.NumRefs()
		sn.EachDead(i, func(f int) {
			for _, k := range seg.SymbolsInFile(f) {
				if seg.SymbolKind(k) != facts.KindEmbed {
					decls--
				}
			}
			refs -= len(seg.RefsInFile(f))
		})
	}
	return decls, refs
}
