package store

import (
	"sync"
	"sync/atomic"

	"github.com/spawn08/chronos-code/internal/indexer/segment"
)

// segRef is an open segment shared by snapshots. The mapping is released and
// the file removed once the segment is retired and no snapshot uses it.
type segRef struct {
	seg     *segment.Segment
	path    string
	refs    atomic.Int32
	retired atomic.Bool // unmap when the last snapshot releases it
	keep    atomic.Bool // retired by Close: unmap but keep the published file
	once    sync.Once
}

func (r *segRef) acquire() { r.refs.Add(1) }

func (r *segRef) release(remove func(string)) {
	if r.refs.Add(-1) == 0 && r.retired.Load() {
		r.once.Do(func() {
			_ = r.seg.Close()
			if !r.keep.Load() {
				remove(r.path)
			}
		})
	}
}

// Ref locates a file record: segment position within the snapshot and file
// index within that segment.
type Ref struct {
	Seg  int32
	File int32
}

type route struct {
	ref     Ref
	deleted bool
}

// Snapshot is an immutable, consistent view of one index generation. Callers
// obtained from Store.Snapshot must call Release.
type Snapshot struct {
	gen    uint64
	segs   []*segRef // oldest first
	routes map[string]route
	live   [][]bool // per segment, per file: this record is the newest for its path
	nLive  int
	refs   atomic.Int32
	remove func(string)
}

func newSnapshot(gen uint64, segs []*segRef, remove func(string)) *Snapshot {
	sn := &Snapshot{gen: gen, segs: segs, routes: map[string]route{}, live: make([][]bool, len(segs)), remove: remove}
	for i := len(segs) - 1; i >= 0; i-- {
		s := segs[i].seg
		sn.live[i] = make([]bool, s.NumFiles())
		for f := 0; f < s.NumFiles(); f++ {
			path := s.FilePath(f)
			if _, seen := sn.routes[path]; seen {
				continue
			}
			deleted := s.FileMeta(f).Deleted
			sn.routes[path] = route{ref: Ref{int32(i), int32(f)}, deleted: deleted}
			if !deleted {
				sn.live[i][f] = true
				sn.nLive++
			}
		}
	}
	sn.retain()
	return sn
}

// withOverlay derives the next snapshot by stacking one new segment on top.
func (sn *Snapshot) withOverlay(gen uint64, top *segRef) *Snapshot {
	segs := append(append(make([]*segRef, 0, len(sn.segs)+1), sn.segs...), top)
	next := &Snapshot{
		gen: gen, segs: segs, routes: make(map[string]route, len(sn.routes)+top.seg.NumFiles()),
		live: append(append(make([][]bool, 0, len(segs)), sn.live...), nil), nLive: sn.nLive, remove: sn.remove,
	}
	for k, v := range sn.routes {
		next.routes[k] = v
	}
	cloned := map[int32]bool{}
	s := top.seg
	topIdx := int32(len(segs) - 1)
	next.live[topIdx] = make([]bool, s.NumFiles())
	for f := 0; f < s.NumFiles(); f++ {
		m := s.FileMeta(f)
		if old, ok := next.routes[m.Path]; ok && !old.deleted {
			if !cloned[old.ref.Seg] {
				next.live[old.ref.Seg] = append([]bool(nil), next.live[old.ref.Seg]...)
				cloned[old.ref.Seg] = true
			}
			next.live[old.ref.Seg][old.ref.File] = false
			next.nLive--
		}
		next.routes[m.Path] = route{ref: Ref{topIdx, int32(f)}, deleted: m.Deleted}
		if !m.Deleted {
			next.live[topIdx][f] = true
			next.nLive++
		}
	}
	next.retain()
	return next
}

func (sn *Snapshot) retain() {
	for _, r := range sn.segs {
		r.acquire()
	}
	sn.refs.Store(1)
}

func (sn *Snapshot) acquire() { sn.refs.Add(1) }

// Release drops the caller's reference.
func (sn *Snapshot) Release() {
	if sn.refs.Add(-1) == 0 {
		for _, r := range sn.segs {
			r.release(sn.remove)
		}
	}
}

// Generation returns the snapshot's generation (0 for an empty index).
func (sn *Snapshot) Generation() uint64 { return sn.gen }

// NumSegments returns the number of segments, oldest first.
func (sn *Snapshot) NumSegments() int { return len(sn.segs) }

// Segment returns segment i (0 = oldest).
func (sn *Snapshot) Segment(i int) *segment.Segment { return sn.segs[i].seg }

// Live reports whether file f of segment i is the current record for its path.
func (sn *Snapshot) Live(i, f int) bool { return sn.live[i][f] }

// NumFiles returns the number of live files.
func (sn *Snapshot) NumFiles() int { return sn.nLive }

// Lookup returns the live record for path.
func (sn *Snapshot) Lookup(path string) (Ref, bool) {
	r, ok := sn.routes[path]
	if !ok || r.deleted {
		return Ref{}, false
	}
	return r.ref, true
}

// Meta returns the metadata of a live file.
func (sn *Snapshot) Meta(path string) (segment.FileMeta, bool) {
	r, ok := sn.Lookup(path)
	if !ok {
		return segment.FileMeta{}, false
	}
	return sn.segs[r.Seg].seg.FileMeta(int(r.File)), true
}

// Paths returns every live path (unordered).
func (sn *Snapshot) Paths() []string {
	out := make([]string, 0, sn.nLive)
	for p, r := range sn.routes {
		if !r.deleted {
			out = append(out, p)
		}
	}
	return out
}

// Refs returns every live file record (unordered).
func (sn *Snapshot) Refs() []Ref {
	out := make([]Ref, 0, sn.nLive)
	for _, r := range sn.routes {
		if !r.deleted {
			out = append(out, r.ref)
		}
	}
	return out
}
