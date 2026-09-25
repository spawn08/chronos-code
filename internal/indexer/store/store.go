// Package store manages index generations on disk: immutable segment files,
// a manifest published by atomic rename, overlay stacking, compaction and a
// single-writer lock. Readers take reference-counted snapshots and never
// block writers.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/segment"
)

// FormatVersion is the manifest format; a mismatch discards the index.
const FormatVersion = 2

const (
	manifestName = "manifest.json"
	segPrefix    = "seg-"
	segSuffix    = ".chx"
	tmpSuffix    = ".tmp"
	lockName     = "writer.lock"
)

// ErrLocked reports that another process holds the writer lock.
var ErrLocked = errors.New("index is locked by another process")

// Manifest lists the segments of the published generation, oldest first.
type Manifest struct {
	Format     int            `json:"format"`
	Generation uint64         `json:"generation"`
	Root       string         `json:"root"`
	Extractor  string         `json:"extractor"`
	Segments   []SegmentEntry `json:"segments"`
}

// SegmentEntry describes one segment file.
type SegmentEntry struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Generation uint64 `json:"generation"`
	Files      int    `json:"files"`
	Bytes      int64  `json:"bytes"`
}

// Store is the writer-side handle for one index directory.
type Store struct {
	dir       string
	root      string
	extractor string
	unlock    func() error

	writeMu sync.Mutex // serializes Publish/Compact
	snapMu  sync.RWMutex
	cur     *Snapshot
	entries []SegmentEntry

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
		refs := make([]*segRef, 0, len(m.Segments))
		for _, e := range m.Segments {
			path := filepath.Join(s.dir, e.Name)
			seg, err := segment.Open(path)
			if err != nil {
				for _, r := range refs {
					_ = r.seg.Close()
				}
				s.reset("segment " + e.Name + ": " + err.Error())
				s.cleanOrphans()
				return nil
			}
			refs = append(refs, &segRef{seg: seg, path: path})
		}
		s.cur = newSnapshot(m.Generation, refs, s.removeFile)
		s.entries = m.Segments
	}
	s.cleanOrphans()
	return nil
}

func (s *Store) reset(reason string) {
	s.Recovered = reason
	s.cur = newSnapshot(0, nil, s.removeFile)
	s.entries = nil
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

// Manifest returns a copy of the published segment list.
func (s *Store) Manifest() Manifest {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	return Manifest{Format: FormatVersion, Generation: s.cur.gen, Root: s.root, Extractor: s.extractor,
		Segments: append([]SegmentEntry(nil), s.entries...)}
}

// Publish writes files as a new generation. With base=true the segment
// replaces every existing segment (files must be the complete live set);
// otherwise it is stacked as an overlay (files are changes, including
// tombstones). An empty overlay publishes nothing.
func (s *Store) Publish(files []*facts.File, base bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.publishLocked(files, base)
}

func (s *Store) publishLocked(files []*facts.File, base bool) error {
	if !base && len(files) == 0 {
		return nil
	}
	s.snapMu.RLock()
	gen := s.cur.gen + 1
	s.snapMu.RUnlock()

	kind, kindName := segment.KindOverlay, "overlay"
	if base {
		kind, kindName = segment.KindBase, "base"
		live := files[:0:0]
		for _, f := range files {
			if !f.Deleted {
				live = append(live, f)
			}
		}
		files = live
	}
	data, err := segment.Encode(files, kind, gen)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s%016x%s", segPrefix, gen, segSuffix)
	path := filepath.Join(s.dir, name)
	if err := writeDurable(path, data); err != nil {
		return fmt.Errorf("write segment: %w", err)
	}
	seg, err := segment.Open(path)
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("reopen segment: %w", err)
	}
	ref := &segRef{seg: seg, path: path}
	entry := SegmentEntry{Name: name, Kind: kindName, Generation: gen, Files: len(files), Bytes: int64(len(data))}

	entries := []SegmentEntry{entry}
	if !base {
		entries = append(append([]SegmentEntry(nil), s.entries...), entry)
	}
	if s.beforeManifest != nil {
		if err := s.beforeManifest(); err != nil {
			_ = seg.Close()
			return err
		}
	}
	m := Manifest{Format: FormatVersion, Generation: gen, Root: s.root, Extractor: s.extractor, Segments: entries}
	if err := writeManifest(filepath.Join(s.dir, manifestName), m); err != nil {
		_ = seg.Close()
		_ = os.Remove(path)
		return fmt.Errorf("publish manifest: %w", err)
	}

	var next *Snapshot
	s.snapMu.Lock()
	prev := s.cur
	if base {
		next = newSnapshot(gen, []*segRef{ref}, s.removeFile)
		for _, r := range prev.segs {
			r.retired.Store(true)
		}
	} else {
		next = prev.withOverlay(gen, ref)
	}
	s.cur, s.entries = next, entries
	s.snapMu.Unlock()
	prev.Release()
	return nil
}

// OverlayStats reports how many overlays sit on the base and their bytes.
func (s *Store) OverlayStats() (overlays int, overlayBytes, baseBytes int64) {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	for _, e := range s.entries {
		if e.Kind == "base" {
			baseBytes += e.Bytes
		} else {
			overlays++
			overlayBytes += e.Bytes
		}
	}
	return overlays, overlayBytes, baseBytes
}

// Compact merges every live record into a single new base segment. Facts
// are copied from the existing segments; no source file is re-read.
func (s *Store) Compact() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	sn := s.Snapshot()
	files := make([]*facts.File, 0, sn.NumFiles())
	for _, r := range sn.Refs() {
		files = append(files, sn.Segment(int(r.Seg)).File(int(r.File)))
	}
	sn.Release()
	return s.publishLocked(files, true)
}

// Close releases the current snapshot and the writer lock. Snapshots still
// held by callers stay valid until released.
func (s *Store) Close() error {
	s.snapMu.Lock()
	cur := s.cur
	s.cur = newSnapshot(0, nil, s.removeFile)
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
