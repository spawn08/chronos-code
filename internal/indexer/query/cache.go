package query

import (
	"slices"
	"strings"
	"sync"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/segment"
	"github.com/spawn08/chronos-code/internal/indexer/store"
)

// Cache holds derived, read-only results shared by the views of one engine.
// Search indexes live in the segments (persisted), so the only cached state
// is the package list of the latest generation, built on first use.
type Cache struct {
	mu    sync.Mutex
	pkgs  *pkgList
	stems map[*segment.Segment]map[string][]int32 // per segment: file stem -> files
	units *unitMemo
	segs  map[*segment.Segment]*segIndex // per segment: manifests, declared packages
	proj  *projMemo                      // project model of the latest generation
}

// unitMemo memoizes import-spec resolution for one generation.
type unitMemo struct {
	gen uint64
	m   map[string][]string
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

// fileStems returns seg's index from file stem (base name without
// extension) to file indexes. Segments are immutable, so each index is
// built once; entries for segments no longer in sn are dropped.
func (c *Cache) fileStems(sn *store.Snapshot, seg *segment.Segment) map[string][]int32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if idx, ok := c.stems[seg]; ok {
		return idx
	}
	if c.stems == nil {
		c.stems = map[*segment.Segment]map[string][]int32{}
	}
	live := map[*segment.Segment]bool{}
	for i := 0; i < sn.NumSegments(); i++ {
		live[sn.Segment(i)] = true
	}
	for s := range c.stems {
		if !live[s] {
			delete(c.stems, s)
		}
	}
	idx := map[string][]int32{}
	for f := 0; f < seg.NumFiles(); f++ {
		p := seg.FilePathView(f)
		base := p[strings.LastIndexByte(p, '/')+1:]
		if dot := strings.IndexByte(base, '.'); dot > 0 {
			base = base[:dot]
		}
		key := strings.Clone(base)
		idx[key] = append(idx[key], int32(f))
	}
	c.stems[seg] = idx
	return idx
}

// memoUnits returns the memoized units of key for sn's generation.
func (c *Cache) memoUnits(gen uint64, key string, compute func() []string) []string {
	c.mu.Lock()
	if c.units == nil || c.units.gen != gen {
		c.units = &unitMemo{gen: gen, m: map[string][]string{}}
	}
	if u, ok := c.units.m[key]; ok {
		c.mu.Unlock()
		return u
	}
	c.mu.Unlock()
	u := compute()
	c.mu.Lock()
	if c.units.gen == gen {
		c.units.m[key] = u
	}
	c.mu.Unlock()
	return u
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
