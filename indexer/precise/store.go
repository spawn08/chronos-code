package precise

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cespare/xxhash/v2"
)

// Store keeps the latest Dir of each package directory, one file per
// directory under its own directory (never inside the workspace). Dirs are
// read lazily and cached; writes replace whole directories atomically.
// Readers may see directories from different loads: every fact is gated
// by file hashes, so mixing loads never yields a stale answer.
type Store struct {
	dir string

	mu      sync.Mutex
	cache   map[string]*Dir // loaded directories; nil entry = known absent
	listed  bool            // every file on disk has been read into cache
	version atomic.Uint64   // bumped by every Put and Delete
}

const (
	fileSuffix  = ".gob"
	versionFile = "VERSION"
)

// OpenStore opens (or creates) a store in dir. A store written by another
// Version is emptied.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("precise: %w", err)
	}
	vf := filepath.Join(dir, versionFile)
	if data, err := os.ReadFile(vf); err != nil || strings.TrimSpace(string(data)) != Version {
		names, _ := os.ReadDir(dir)
		for _, n := range names {
			_ = os.Remove(filepath.Join(dir, n.Name()))
		}
		if err := os.WriteFile(vf, []byte(Version+"\n"), 0o644); err != nil {
			return nil, fmt.Errorf("precise: %w", err)
		}
	}
	return &Store{dir: dir, cache: map[string]*Dir{}}, nil
}

// Version counts changes; a reader can cache derived data per version.
func (s *Store) Version() uint64 { return s.version.Load() }

func (s *Store) file(dir string) string {
	return filepath.Join(s.dir, fmt.Sprintf("%016x", xxhash.Sum64String(dir))+fileSuffix)
}

// Get returns the facts of a package directory, or nil.
func (s *Store) Get(dir string) *Dir {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.cache[dir]; ok {
		return d
	}
	d, err := readDir(s.file(dir))
	if err != nil || d.Path != dir {
		d = nil // absent, unreadable or a hash collision: no facts
	}
	s.cache[dir] = d
	return d
}

func readDir(name string) (*Dir, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var d Dir
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

// All returns every stored directory, sorted by path.
func (s *Store) All() []*Dir {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.listed {
		names, _ := os.ReadDir(s.dir)
		for _, n := range names {
			if !strings.HasSuffix(n.Name(), fileSuffix) {
				continue
			}
			if d, err := readDir(filepath.Join(s.dir, n.Name())); err == nil {
				if _, ok := s.cache[d.Path]; !ok {
					s.cache[d.Path] = d
				}
			}
		}
		s.listed = true
	}
	out := make([]*Dir, 0, len(s.cache))
	for _, d := range s.cache {
		if d != nil {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b *Dir) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// Put stores dirs, replacing their previous facts.
func (s *Store) Put(dirs []*Dir) error {
	var errs []error
	for _, d := range dirs {
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(d); err != nil {
			errs = append(errs, err)
			continue
		}
		name := s.file(d.Path)
		tmp := name + ".tmp"
		if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(tmp, name); err != nil {
			_ = os.Remove(tmp)
			errs = append(errs, err)
			continue
		}
		s.mu.Lock()
		s.cache[d.Path] = d
		s.mu.Unlock()
	}
	s.version.Add(1)
	if len(errs) > 0 {
		return fmt.Errorf("precise: store: %w", errors.Join(errs...))
	}
	return nil
}

// Delete removes the facts of dirs.
func (s *Store) Delete(dirs []string) {
	for _, dir := range dirs {
		_ = os.Remove(s.file(dir))
		s.mu.Lock()
		s.cache[dir] = nil
		s.mu.Unlock()
	}
	s.version.Add(1)
}
