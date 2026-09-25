package query

import (
	"slices"
	"sync"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/segment"
	"github.com/spawn08/chronos-code/internal/indexer/store"
)

// Cache holds derived, read-only structures shared by views of one engine:
// search indexes per immutable segment (built once, dropped when the segment
// leaves the current snapshot) and the package index of the latest
// generation. Safe for concurrent use.
type Cache struct {
	mu   sync.Mutex
	segs map[*segment.Segment]*segEntry
	pkgs *pkgIndex
}

type segEntry struct {
	once sync.Once
	idx  *segIndex
}

type pkgIndex struct {
	gen     uint64
	names   []string
	files   map[string][]string // package -> sorted live paths
	symbols int
	calls   int
}

// NewCache returns an empty cache.
func NewCache() *Cache { return &Cache{segs: map[*segment.Segment]*segEntry{}} }

// segment returns the search index of segment i of sn, building it once.
// Entries for segments no longer in sn are dropped.
func (c *Cache) segment(sn *store.Snapshot, i int) *segIndex {
	seg := sn.Segment(i)
	c.mu.Lock()
	e := c.segs[seg]
	if e == nil {
		e = &segEntry{}
		c.segs[seg] = e
		if len(c.segs) > sn.NumSegments() {
			live := make(map[*segment.Segment]bool, sn.NumSegments())
			for j := 0; j < sn.NumSegments(); j++ {
				live[sn.Segment(j)] = true
			}
			for s := range c.segs {
				if !live[s] {
					delete(c.segs, s)
				}
			}
		}
	}
	c.mu.Unlock()
	e.once.Do(func() { e.idx = buildSegIndex(seg) })
	return e.idx
}

func (c *Cache) packages(sn *store.Snapshot) *pkgIndex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pkgs != nil && c.pkgs.gen == sn.Generation() {
		return c.pkgs
	}
	idx := &pkgIndex{gen: sn.Generation(), files: map[string][]string{}}
	for _, ref := range sn.Refs() {
		seg := sn.Segment(int(ref.Seg))
		m := seg.FileMeta(int(ref.File))
		idx.files[m.Package] = append(idx.files[m.Package], m.Path)
		for _, k := range seg.SymbolsInFile(int(ref.File)) {
			if seg.SymbolKind(k) != facts.KindEmbed {
				idx.symbols++
			}
		}
		idx.calls += len(seg.CallsInFile(int(ref.File)))
	}
	for p, files := range idx.files {
		slices.Sort(files)
		idx.names = append(idx.names, p)
	}
	slices.Sort(idx.names)
	c.pkgs = idx
	return idx
}
