package store

import (
	"sort"
	"strings"
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
//
// Segments are the base shards (disjoint path ranges, ordered by their lower
// bound; shard 0 starts at "") followed by overlays, oldest first. Routing is
// layered: a path is looked up in the overlay map (small, bounded by
// compaction), then by binary search in the one shard whose range holds it.
// Records shadowed by a newer overlay, and tombstones, are kept in sparse
// per-segment dead sets. Deriving a snapshot for a new overlay therefore
// costs O(overlay files), never O(repository files).
type Snapshot struct {
	gen     uint64
	segs    []*segRef
	los     []string // lower bound of each shard
	nShards int
	over    map[string]route     // newest overlay record per path, tombstones included
	dead    []map[int32]struct{} // per segment; nil when nothing in it is dead
	nLive   int
	work    int // route and dead-set entries touched deriving this snapshot
	refs    atomic.Int32
	remove  func(string)
}

// newSnapshot builds a snapshot from shards (ordered by lo) and overlays
// (oldest first). Its cost is O(shards + overlay files).
func newSnapshot(gen uint64, shards []*segRef, los []string, overlays []*segRef, remove func(string)) *Snapshot {
	sn := &Snapshot{
		gen: gen, segs: append(append([]*segRef(nil), shards...), overlays...), los: los, nShards: len(shards),
		over: map[string]route{}, remove: remove,
	}
	sn.dead = make([]map[int32]struct{}, len(sn.segs))
	for _, r := range shards {
		sn.nLive += r.seg.NumFiles()
	}
	cloned := map[int]bool{}
	for i := range overlays {
		sn.apply(len(shards)+i, cloned)
	}
	sn.retain()
	return sn
}

// withOverlay derives the next snapshot by stacking one new segment on top.
func (sn *Snapshot) withOverlay(gen uint64, top *segRef) *Snapshot {
	next := &Snapshot{
		gen: gen, segs: append(append(make([]*segRef, 0, len(sn.segs)+1), sn.segs...), top),
		los: sn.los, nShards: sn.nShards, over: make(map[string]route, len(sn.over)+top.seg.NumFiles()),
		dead:  append(append(make([]map[int32]struct{}, 0, len(sn.segs)+1), sn.dead...), nil),
		nLive: sn.nLive, remove: sn.remove,
	}
	for k, v := range sn.over {
		next.over[k] = v
	}
	next.work = len(sn.over)
	next.apply(len(next.segs)-1, map[int]bool{})
	next.retain()
	return next
}

// apply routes the files of overlay segment ti over everything older.
// Dead sets are copied on first write (cloned tracks which are private).
func (sn *Snapshot) apply(ti int, cloned map[int]bool) {
	markDead := func(seg int, file int32) {
		if !cloned[seg] {
			d := make(map[int32]struct{}, len(sn.dead[seg])+1)
			for k := range sn.dead[seg] {
				d[k] = struct{}{}
			}
			sn.work += len(sn.dead[seg])
			sn.dead[seg] = d
			cloned[seg] = true
		}
		sn.dead[seg][file] = struct{}{}
	}
	seg := sn.segs[ti].seg
	for f := 0; f < seg.NumFiles(); f++ {
		path := seg.FilePath(f)
		deleted := seg.FileDeleted(f)
		sn.work++
		if old, ok := sn.over[path]; ok {
			if !old.deleted {
				markDead(int(old.ref.Seg), old.ref.File)
				sn.nLive--
			}
		} else if k, fi, ok := sn.baseLookup(path); ok && sn.Live(k, fi) {
			markDead(k, int32(fi))
			sn.nLive--
		}
		sn.over[path] = route{ref: Ref{int32(ti), int32(f)}, deleted: deleted}
		if deleted {
			markDead(ti, int32(f))
		} else {
			sn.nLive++
		}
	}
}

// ShardFor returns the shard whose range holds path, or -1 without shards.
func (sn *Snapshot) ShardFor(path string) int {
	if sn.nShards == 0 {
		return -1
	}
	return max(0, sort.Search(sn.nShards, func(k int) bool { return sn.los[k] > path })-1)
}

func (sn *Snapshot) baseLookup(path string) (shard, file int, ok bool) {
	k := sn.ShardFor(path)
	if k < 0 {
		return 0, 0, false
	}
	f, found := sn.segs[k].seg.FindFile(path)
	return k, f, found
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

// NumSegments returns the number of segments: shards first, then overlays.
func (sn *Snapshot) NumSegments() int { return len(sn.segs) }

// NumShards returns the number of base shards (segments 0..NumShards-1).
func (sn *Snapshot) NumShards() int { return sn.nShards }

// ShardLo returns the lower bound of shard k's path range.
func (sn *Snapshot) ShardLo(k int) string { return sn.los[k] }

// NumOverlayFiles returns the number of routed overlay records.
func (sn *Snapshot) NumOverlayFiles() int { return len(sn.over) }

// BuildWork returns the route and dead-set entries touched when this
// snapshot was derived. It measures the edit path's cost and must not grow
// with the number of files in the base.
func (sn *Snapshot) BuildWork() int { return sn.work }

// Segment returns segment i.
func (sn *Snapshot) Segment(i int) *segment.Segment { return sn.segs[i].seg }

// Live reports whether file f of segment i is the current record for its
// path (not shadowed by a newer overlay, and not a tombstone).
func (sn *Snapshot) Live(i, f int) bool {
	d := sn.dead[i]
	if d == nil {
		return true
	}
	_, gone := d[int32(f)]
	return !gone
}

// NumFiles returns the number of live files.
func (sn *Snapshot) NumFiles() int { return sn.nLive }

// Lookup returns the live record for path.
func (sn *Snapshot) Lookup(path string) (Ref, bool) {
	if r, ok := sn.over[path]; ok {
		return r.ref, !r.deleted
	}
	k, f, ok := sn.baseLookup(path)
	if !ok {
		return Ref{}, false
	}
	return Ref{int32(k), int32(f)}, true
}

// Meta returns the metadata of a live file.
func (sn *Snapshot) Meta(path string) (segment.FileMeta, bool) {
	r, ok := sn.Lookup(path)
	if !ok {
		return segment.FileMeta{}, false
	}
	return sn.segs[r.Seg].seg.FileMeta(int(r.File)), true
}

// OverlayPaths returns the paths routed to overlays, tombstones included.
func (sn *Snapshot) OverlayPaths() []string {
	out := make([]string, 0, len(sn.over))
	for p := range sn.over {
		out = append(out, p)
	}
	return out
}

// HasPrefix reports whether any live path starts with prefix. Its cost is
// O(overlay files + log(files)).
func (sn *Snapshot) HasPrefix(prefix string) bool {
	for p, r := range sn.over {
		if !r.deleted && strings.HasPrefix(p, prefix) {
			return true
		}
	}
	for k := max(0, sn.ShardFor(prefix)); k < sn.nShards; k++ {
		if k > 0 && sn.los[k] > prefix && !strings.HasPrefix(sn.los[k], prefix) {
			break
		}
		seg := sn.segs[k].seg
		for f, _ := seg.FindFile(prefix); f < seg.NumFiles(); f++ {
			if !strings.HasPrefix(seg.FilePathView(f), prefix) {
				break
			}
			if sn.Live(k, f) {
				return true
			}
		}
	}
	return false
}

// Paths returns every live path. It is O(files): full reconciles only.
func (sn *Snapshot) Paths() []string {
	out := make([]string, 0, sn.nLive)
	for _, r := range sn.Refs() {
		out = append(out, sn.segs[r.Seg].seg.FilePath(int(r.File)))
	}
	return out
}

// Refs returns every live file record, shards in path order then overlays.
// It is O(files): full reconciles and compaction only.
func (sn *Snapshot) Refs() []Ref {
	out := make([]Ref, 0, sn.nLive)
	for i, r := range sn.segs {
		for f := 0; f < r.seg.NumFiles(); f++ {
			if sn.Live(i, f) {
				out = append(out, Ref{int32(i), int32(f)})
			}
		}
	}
	return out
}

// EachDead calls fn for each dead file of segment i (shadowed or tombstone).
func (sn *Snapshot) EachDead(i int, fn func(f int)) {
	for f := range sn.dead[i] {
		fn(int(f))
	}
}
