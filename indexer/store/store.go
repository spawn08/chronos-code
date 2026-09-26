// Package store manages index generations on disk: immutable segment files,
// a manifest published by atomic rename, a base split into path-range
// shards, overlay stacking, per-shard compaction and a single-writer lock.
// Readers take reference-counted snapshots and never block writers.
package store

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/segment"
)

// FormatVersion is the manifest format; a mismatch discards the index.
// v3: sharded base and index metadata.
const FormatVersion = 3

// DefaultShardBytes is the estimated fact volume per base shard. Compaction
// rewrites only the shards an overlay touches, so this bounds its cost.
const DefaultShardBytes = 8 << 20

const (
	manifestName = "manifest.json"
	segPrefix    = "seg-"
	segSuffix    = ".chx"
	tmpSuffix    = ".tmp"
	lockName     = "writer.lock"
	kindShard    = "shard"
	kindOverlay  = "overlay"
)

// ErrLocked reports that another process holds the writer lock.
var ErrLocked = errors.New("index is locked by another process")

// Meta is index state the engine persists with each generation.
type Meta struct {
	// Complete is false while a first build is still in progress (a
	// progressive build publishes the working set first).
	Complete bool `json:"complete"`
	// Commit is the git HEAD the index was last reconciled against.
	Commit string `json:"commit,omitempty"`
	// Touched lists paths that may differ from Commit's content in the
	// index: dirty at the last reconcile, or updated since.
	Touched []string `json:"touched,omitempty"`
	// TouchedOverflow is set when Touched outgrew its cap; the next
	// reconcile must list the whole workspace.
	TouchedOverflow bool `json:"touched_overflow,omitempty"`
	// Modules maps each go.mod directory to its module path.
	Modules map[string]string `json:"modules,omitempty"`
}

// Manifest lists the segments of the published generation: shards ordered
// by their lower bound, then overlays, oldest first.
type Manifest struct {
	Format     int            `json:"format"`
	Generation uint64         `json:"generation"`
	Root       string         `json:"root"`
	Extractor  string         `json:"extractor"`
	Segments   []SegmentEntry `json:"segments"`
	Meta       Meta           `json:"meta"`
}

// SegmentEntry describes one segment file.
type SegmentEntry struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Lo         string `json:"lo,omitempty"` // shards: lower bound of the path range
	Generation uint64 `json:"generation"`
	Files      int    `json:"files"`
	Bytes      int64  `json:"bytes"`
}

// Store is the writer-side handle for one index directory.
type Store struct {
	dir        string
	root       string
	extractor  string
	unlock     func() error
	ShardBytes int // estimated fact bytes per shard; 0 = DefaultShardBytes

	writeMu sync.Mutex // serializes Publish, base builds, Compact, Import
	snapMu  sync.RWMutex
	cur     *Snapshot
	entries []SegmentEntry
	meta    Meta

	// Recovered is non-empty when Open discarded an unusable index.
	Recovered string

	// beforeManifest, when set by tests, runs after the segment is durable and
	// before the manifest is published.
	beforeManifest func() error
}

// Open acquires the writer lock on dir and loads the published generation.
// A missing, foreign (other root or extractor) or corrupt index is discarded
// and Open returns an empty store with Recovered set; the caller rebuilds.
func Open(dir, root, extractor string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create index dir: %w", err)
	}
	unlock, err := lockDir(filepath.Join(dir, lockName))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, root: root, extractor: extractor, unlock: unlock}
	if err := s.load(); err != nil {
		_ = unlock()
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	m, err := readManifest(filepath.Join(s.dir, manifestName))
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.reset("")
	case err != nil:
		s.reset("unreadable manifest: " + err.Error())
	case m.Format != FormatVersion || m.Root != s.root || m.Extractor != s.extractor:
		s.reset("index format, root or extractor changed")
	default:
		shards, los, overlays, err := s.openSegments(m.Segments)
		if err != nil {
			s.reset(err.Error())
			s.cleanOrphans()
			return nil
		}
		s.cur = newSnapshot(m.Generation, shards, los, overlays, s.removeFile)
		s.entries, s.meta = m.Segments, m.Meta
	}
	s.cleanOrphans()
	return nil
}

func (s *Store) openSegments(entries []SegmentEntry) (shards []*segRef, los []string, overlays []*segRef, err error) {
	closeAll := func() {
		for _, r := range append(shards, overlays...) {
			_ = r.seg.Close()
		}
	}
	// Segments are validated on open (checksums and every record), which is
	// O(bytes); they are independent, so validate them in parallel.
	segs := make([]*segment.Segment, len(entries))
	errs := make([]error, len(entries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			segs[i], errs[i] = segment.Open(filepath.Join(s.dir, e.Name))
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		for _, seg := range segs {
			if seg != nil {
				_ = seg.Close()
			}
		}
		return nil, nil, nil, fmt.Errorf("open segments: %w", err)
	}
	for i, e := range entries {
		path := filepath.Join(s.dir, e.Name)
		ref := &segRef{seg: segs[i], path: path}
		switch {
		case e.Kind == kindShard && len(overlays) == 0 && (len(los) == 0 && e.Lo == "" || len(los) > 0 && e.Lo > los[len(los)-1]):
			shards, los = append(shards, ref), append(los, e.Lo)
		case e.Kind == kindOverlay:
			overlays = append(overlays, ref)
		default:
			shards = append(shards, ref)
			closeAll()
			return nil, nil, nil, fmt.Errorf("manifest segment %d out of order", i)
		}
	}
	return shards, los, overlays, nil
}

func (s *Store) reset(reason string) {
	s.Recovered = reason
	s.cur = newSnapshot(0, nil, nil, nil, s.removeFile)
	s.entries, s.meta = nil, Meta{}
	_ = os.Remove(filepath.Join(s.dir, manifestName))
}

// cleanOrphans removes segment and temp files the manifest does not reference
// (left by a crash before publication, or by a discarded index).
func (s *Store) cleanOrphans() {
	keep := map[string]bool{}
	for _, e := range s.entries {
		keep[e.Name] = true
	}
	names, _ := os.ReadDir(s.dir)
	for _, d := range names {
		n := d.Name()
		if strings.HasSuffix(n, tmpSuffix) || (strings.HasPrefix(n, segPrefix) && strings.HasSuffix(n, segSuffix) && !keep[n]) {
			_ = os.Remove(filepath.Join(s.dir, n))
		}
	}
}

func (s *Store) removeFile(path string) { _ = os.Remove(path) }

// Dir returns the index directory.
func (s *Store) Dir() string { return s.dir }

// Snapshot returns the current generation. Call Release when done.
func (s *Store) Snapshot() *Snapshot {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	s.cur.acquire()
	return s.cur
}

// Manifest returns a copy of the published manifest.
func (s *Store) Manifest() Manifest {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	return s.manifestLocked()
}

func (s *Store) manifestLocked() Manifest {
	m := s.meta
	m.Touched = slices.Clone(m.Touched)
	return Manifest{Format: FormatVersion, Generation: s.cur.gen, Root: s.root, Extractor: s.extractor,
		Segments: slices.Clone(s.entries), Meta: m}
}

// Meta returns the current index metadata.
func (s *Store) Meta() Meta { return s.Manifest().Meta }

// StageMeta replaces the metadata; it is persisted with the next publish.
func (s *Store) StageMeta(m Meta) {
	s.snapMu.Lock()
	s.meta = m
	s.snapMu.Unlock()
}

// SetMeta replaces the metadata and persists it now.
func (s *Store) SetMeta(m Meta) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.snapMu.Lock()
	s.meta = m
	man := s.manifestLocked()
	s.snapMu.Unlock()
	if man.Generation == 0 {
		return nil // nothing published yet; the first publish persists it
	}
	return writeManifest(filepath.Join(s.dir, manifestName), man)
}

func (s *Store) shardBytes() int {
	if s.ShardBytes > 0 {
		return s.ShardBytes
	}
	return DefaultShardBytes
}

// Publish writes files as a new generation. With base=true the files are
// the complete live set and replace every segment (as shards); otherwise
// they are stacked as an overlay (changes, including tombstones). An empty
// overlay publishes nothing.
func (s *Store) Publish(files []*facts.File, base bool) error {
	if base {
		sorted := slices.Clone(files)
		slices.SortFunc(sorted, func(a, b *facts.File) int { return cmp.Compare(a.Path, b.Path) })
		w := s.NewBase()
		for _, f := range sorted {
			if err := w.Add(f); err != nil {
				w.Abort()
				return err
			}
		}
		return w.Commit()
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if len(files) == 0 {
		return nil
	}
	gen := s.nextGen()
	ref, entry, err := s.writeSegment(files, segment.KindOverlay, kindOverlay, gen, fmt.Sprintf("%016x", gen))
	if err != nil {
		return err
	}
	s.snapMu.RLock()
	entries := append(slices.Clone(s.entries), entry)
	s.snapMu.RUnlock()
	if err := s.publishManifest(gen, entries, []*segRef{ref}); err != nil {
		return err
	}
	s.swap(gen, entries, func(prev *Snapshot) *Snapshot { return prev.withOverlay(gen, ref) })
	return nil
}

func (s *Store) nextGen() uint64 {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	return s.cur.gen + 1
}

// writeSegment encodes, durably writes and reopens one segment.
func (s *Store) writeSegment(files []*facts.File, kind segment.Kind, kindName string, gen uint64, suffix string) (*segRef, SegmentEntry, error) {
	data, err := segment.Encode(files, kind, gen)
	if err != nil {
		return nil, SegmentEntry{}, err
	}
	name := segPrefix + suffix + segSuffix
	path := filepath.Join(s.dir, name)
	if err := writeDurable(path, data); err != nil {
		return nil, SegmentEntry{}, fmt.Errorf("write segment: %w", err)
	}
	seg, err := segment.Open(path)
	if err != nil {
		_ = os.Remove(path)
		return nil, SegmentEntry{}, fmt.Errorf("reopen segment: %w", err)
	}
	return &segRef{seg: seg, path: path}, SegmentEntry{Name: name, Kind: kindName, Generation: gen, Files: len(files), Bytes: int64(len(data))}, nil
}

// publishManifest writes the manifest for gen; on failure the new
// segments are closed and removed.
func (s *Store) publishManifest(gen uint64, entries []SegmentEntry, fresh []*segRef) error {
	fail := func(err error) error {
		for _, r := range fresh {
			_ = r.seg.Close()
			_ = os.Remove(r.path)
		}
		return err
	}
	if s.beforeManifest != nil {
		if err := s.beforeManifest(); err != nil {
			return fail(err)
		}
	}
	s.snapMu.RLock()
	meta := s.meta
	s.snapMu.RUnlock()
	m := Manifest{Format: FormatVersion, Generation: gen, Root: s.root, Extractor: s.extractor, Segments: entries, Meta: meta}
	if err := writeManifest(filepath.Join(s.dir, manifestName), m); err != nil {
		return fail(fmt.Errorf("publish manifest: %w", err))
	}
	return nil
}

func (s *Store) swap(gen uint64, entries []SegmentEntry, next func(prev *Snapshot) *Snapshot) {
	s.snapMu.Lock()
	prev := s.cur
	s.cur, s.entries = next(prev), entries
	s.snapMu.Unlock()
	prev.Release()
}

// OverlayStats reports how many overlays sit on the base and their bytes.
func (s *Store) OverlayStats() (overlays int, overlayBytes, baseBytes int64) {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	for _, e := range s.entries {
		if e.Kind == kindShard {
			baseBytes += e.Bytes
		} else {
			overlays++
			overlayBytes += e.Bytes
		}
	}
	return overlays, overlayBytes, baseBytes
}

// BaseWriter streams a complete live file set into base shards: files are
// added in increasing path order and written a shard at a time, so a build
// never holds more than one shard's facts in memory.
type BaseWriter struct {
	s       *Store
	gen     uint64
	pending []*facts.File
	bytes   int
	lastErr error
	prev    string
	shards  []*segRef
	entries []SegmentEntry
	done    bool
}

// NewBase starts a base build. It holds the writer until Commit or Abort.
func (s *Store) NewBase() *BaseWriter {
	s.writeMu.Lock()
	return &BaseWriter{s: s, gen: s.nextGen()}
}

// Add appends one live file; paths must be strictly increasing.
func (w *BaseWriter) Add(f *facts.File) error {
	if f.Deleted {
		return nil
	}
	if len(w.entries)+len(w.pending) > 0 && f.Path <= w.prev {
		return fmt.Errorf("base writer: path %q out of order", f.Path)
	}
	w.prev = f.Path
	w.pending = append(w.pending, f)
	w.bytes += EstimateBytes(f)
	if w.bytes >= w.s.shardBytes() {
		return w.flush()
	}
	return nil
}

// EstimateBytes approximates a file's encoded size, to size shards.
func EstimateBytes(f *facts.File) int {
	n := 112 + len(f.Path) + len(f.Package)
	for _, s := range f.Symbols {
		n += 56 + len(s.Name) + len(s.Signature) + len(s.Doc) + len(s.Receiver) + 64
	}
	for _, im := range f.Imports {
		n += 40 + 16*len(im.Names)
	}
	return n + 32*len(f.Refs) + 32*len(f.Exports) + 32*len(f.Hints)
}

func (w *BaseWriter) flush() error {
	lo := ""
	if len(w.shards) > 0 {
		lo = w.pending[0].Path
	}
	ref, entry, err := w.s.writeSegment(w.pending, segment.KindBase, kindShard, w.gen, fmt.Sprintf("%016x-%05d", w.gen, len(w.shards)))
	if err != nil {
		return err
	}
	entry.Lo = lo
	w.shards, w.entries = append(w.shards, ref), append(w.entries, entry)
	w.pending, w.bytes = nil, 0
	return nil
}

// Commit writes the last shard and publishes the base, replacing every
// existing segment.
func (w *BaseWriter) Commit() error {
	if w.done {
		return errors.New("base writer: already finished")
	}
	defer w.finish()
	if len(w.pending) > 0 || len(w.shards) == 0 {
		if err := w.flush(); err != nil {
			w.discard()
			return err
		}
	}
	if err := w.s.publishManifest(w.gen, w.entries, w.shards); err != nil {
		return err
	}
	los := make([]string, len(w.entries))
	for i, e := range w.entries {
		los[i] = e.Lo
	}
	w.s.swap(w.gen, w.entries, func(prev *Snapshot) *Snapshot {
		for _, r := range prev.segs {
			r.retired.Store(true)
		}
		return newSnapshot(w.gen, w.shards, los, nil, w.s.removeFile)
	})
	return nil
}

// Abort discards the shards written so far.
func (w *BaseWriter) Abort() {
	if w.done {
		return
	}
	w.discard()
	w.finish()
}

func (w *BaseWriter) discard() {
	for _, r := range w.shards {
		_ = r.seg.Close()
		_ = os.Remove(r.path)
	}
	w.shards = nil
}

func (w *BaseWriter) finish() {
	w.done = true
	w.s.writeMu.Unlock()
}

// Compact folds the overlays into the base. Only the shards whose path range
// holds an overlay record are rewritten (from stored facts; no source file
// is re-read); a shard that grows past the size target is split and an
// emptied shard is dropped. Its cost is proportional to the touched shards,
// not the repository.
func (s *Store) Compact() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	sn := s.Snapshot()
	defer sn.Release()
	if sn.NumSegments() == sn.NumShards() {
		return nil
	}
	gen := sn.Generation() + 1
	// Overlay records by shard (shard -1 when there is no base yet).
	byShard := map[int][]*facts.File{}
	touched := map[int]bool{}
	for _, p := range sn.OverlayPaths() {
		k := sn.ShardFor(p)
		touched[k] = true
		if ref, ok := sn.Lookup(p); ok {
			byShard[k] = append(byShard[k], sn.Segment(int(ref.Seg)).File(int(ref.File)))
		}
	}
	type piece struct {
		lo    string
		ref   *segRef
		entry SegmentEntry
		fresh bool
	}
	var pieces []piece
	var fresh, replaced []*segRef
	n := 0
	// build writes files (one old shard's range, starting at lo) as one or
	// more new shards; the first keeps lo, later ones start at their first path.
	build := func(lo string, files []*facts.File) error {
		slices.SortFunc(files, func(a, b *facts.File) int { return cmp.Compare(a.Path, b.Path) })
		var chunk []*facts.File
		bytes, first := 0, true
		emit := func() error {
			if len(chunk) == 0 {
				return nil
			}
			plo := chunk[0].Path
			if first {
				plo, first = lo, false
			}
			ref, entry, err := s.writeSegment(chunk, segment.KindBase, kindShard, gen, fmt.Sprintf("%016x-%05d", gen, n))
			if err != nil {
				return err
			}
			n++
			entry.Lo = plo
			fresh = append(fresh, ref)
			pieces = append(pieces, piece{plo, ref, entry, true})
			chunk, bytes = nil, 0
			return nil
		}
		for _, f := range files {
			chunk = append(chunk, f)
			if bytes += EstimateBytes(f); bytes >= s.shardBytes() {
				if err := emit(); err != nil {
					return err
				}
			}
		}
		return emit()
	}
	fail := func(err error) error {
		for _, r := range fresh {
			_ = r.seg.Close()
			_ = os.Remove(r.path)
		}
		return err
	}
	if touched[-1] {
		if err := build("", byShard[-1]); err != nil {
			return fail(err)
		}
	}
	s.snapMu.RLock()
	oldEntries := slices.Clone(s.entries)
	s.snapMu.RUnlock()
	for k := 0; k < sn.NumShards(); k++ {
		if !touched[k] {
			pieces = append(pieces, piece{lo: sn.ShardLo(k), ref: sn.segs[k], entry: oldEntries[k]})
			continue
		}
		seg := sn.Segment(k)
		files := byShard[k]
		for f := 0; f < seg.NumFiles(); f++ {
			if sn.Live(k, f) {
				files = append(files, seg.File(f))
			}
		}
		replaced = append(replaced, sn.segs[k])
		if err := build(sn.ShardLo(k), files); err != nil {
			return fail(err)
		}
	}
	slices.SortStableFunc(pieces, func(a, b piece) int { return cmp.Compare(a.lo, b.lo) })
	if len(pieces) > 0 {
		pieces[0].lo, pieces[0].entry.Lo = "", ""
	} else {
		// Everything was deleted: keep one empty shard as the base.
		ref, entry, err := s.writeSegment(nil, segment.KindBase, kindShard, gen, fmt.Sprintf("%016x-%05d", gen, n))
		if err != nil {
			return fail(err)
		}
		fresh = append(fresh, ref)
		pieces = append(pieces, piece{"", ref, entry, true})
	}
	entries := make([]SegmentEntry, len(pieces))
	shards := make([]*segRef, len(pieces))
	los := make([]string, len(pieces))
	for i, p := range pieces {
		entries[i], shards[i], los[i] = p.entry, p.ref, p.lo
	}
	if err := s.publishManifest(gen, entries, fresh); err != nil {
		return err
	}
	s.swap(gen, entries, func(prev *Snapshot) *Snapshot {
		for _, r := range prev.segs[prev.nShards:] {
			r.retired.Store(true)
		}
		for _, r := range replaced {
			r.retired.Store(true)
		}
		return newSnapshot(gen, shards, los, nil, s.removeFile)
	})
	return nil
}

// Import replaces the index with a copy of the index in src (built for
// another checkout of the same repository, e.g. by CI). Paths are
// root-relative, so only the manifest's root is rewritten. The extractor
// must match. The caller reconciles afterwards; with the source's Commit in
// the metadata, that indexes only the difference.
func (s *Store) Import(src string) error {
	m, err := readManifest(filepath.Join(src, manifestName))
	if err != nil {
		return fmt.Errorf("read prebuilt manifest: %w", err)
	}
	if m.Format != FormatVersion || m.Extractor != s.extractor {
		return fmt.Errorf("prebuilt index format %d / extractor %q does not match %d / %q", m.Format, m.Extractor, FormatVersion, s.extractor)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	gen := s.nextGen()
	var fresh []*segRef
	fail := func(err error) error {
		for _, r := range fresh {
			_ = r.seg.Close()
			_ = os.Remove(r.path)
		}
		return err
	}
	entries := make([]SegmentEntry, len(m.Segments))
	for i, e := range m.Segments {
		name := fmt.Sprintf("%s%016x-i%05d%s", segPrefix, gen, i, segSuffix)
		dst := filepath.Join(s.dir, name)
		if err := copyDurable(filepath.Join(src, e.Name), dst); err != nil {
			return fail(fmt.Errorf("copy %s: %w", e.Name, err))
		}
		seg, err := segment.Open(dst)
		if err != nil {
			_ = os.Remove(dst)
			return fail(fmt.Errorf("prebuilt segment %s: %w", e.Name, err))
		}
		fresh = append(fresh, &segRef{seg: seg, path: dst})
		e.Name = name
		entries[i] = e
	}
	for _, r := range fresh {
		_ = r.seg.Close()
	}
	shards, los, overlays, err := s.openSegments(entries)
	if err != nil {
		return fail(err)
	}
	fresh = append(shards, overlays...)
	s.snapMu.Lock()
	s.meta = m.Meta
	s.snapMu.Unlock()
	if err := s.publishManifest(gen, entries, fresh); err != nil {
		return err
	}
	s.swap(gen, entries, func(prev *Snapshot) *Snapshot {
		for _, r := range prev.segs {
			r.retired.Store(true)
		}
		return newSnapshot(gen, shards, los, overlays, s.removeFile)
	})
	return nil
}

func copyDurable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + tmpSuffix
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := syncFile(out); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// Close releases the current snapshot and the writer lock. Snapshots still
// held by callers stay valid until released.
func (s *Store) Close() error {
	s.snapMu.Lock()
	cur := s.cur
	s.cur = newSnapshot(0, nil, nil, nil, s.removeFile)
	s.snapMu.Unlock()
	for _, r := range cur.segs {
		r.keep.Store(true) // still published: unmap only
		r.retired.Store(true)
	}
	cur.Release()
	return s.unlock()
}

func readManifest(path string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

func writeManifest(path string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeDurable(path, data)
}

// writeDurable writes data to path via a synced temp file, rename, and a
// directory sync, so readers see either the old file or the complete new one.
func writeDurable(path string, data []byte) error {
	tmp := path + tmpSuffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := syncFile(f); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}
