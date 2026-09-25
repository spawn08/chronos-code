// Package indexer is the chronos indexer: a syntactic, incremental code index
// whose edit path never type-checks. Files are parsed once, their facts are
// written as immutable segments, and queries read reference-counted
// snapshots. See docs/chronos-indexer.md.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"

	"github.com/spawn08/chronos-code/internal/indexer/extract/golang"
	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/scan"
	"github.com/spawn08/chronos-code/internal/indexer/store"
)

// ExtractorVersion changes whenever extracted facts change shape or meaning;
// an index written by another version is discarded and rebuilt.
const ExtractorVersion = "go-syntax-1"

// Compaction thresholds: merge overlays into a new base when either is hit.
const (
	maxOverlays        = 16
	maxOverlayFraction = 4 // overlay bytes > base bytes / maxOverlayFraction
	minOverlaysForSize = 4
	maxFileBytes       = 4 << 20
)

// Options configures an Engine.
type Options struct {
	Root    string // absolute workspace root
	Dir     string // absolute index directory, outside Root
	Workers int    // parse workers; 0 = GOMAXPROCS
	Logf    func(format string, args ...any)
}

// Stats describes one indexing pass.
type Stats struct {
	Generation uint64
	Full       bool // published a new base segment
	Scanned    int  // candidate paths examined
	Parsed     int  // files read and extracted
	Deleted    int  // tombstones written
	List       time.Duration
	Stat       time.Duration
	Parse      time.Duration
	Publish    time.Duration
	Total      time.Duration
}

// Engine owns one workspace index.
type Engine struct {
	opts Options
	st   *store.Store

	mu      sync.Mutex        // serializes indexing passes
	modules map[string]string // go.mod dir (root-relative, "." for root) -> module path
	known   map[string]bool   // indexable paths seen in the last listing or update

	compacting atomic.Bool
	wg         sync.WaitGroup
	closed     atomic.Bool
}

// Open opens (or creates) the index for opts.Root. It does not scan; call
// Reconcile to bring the index up to date.
func Open(opts Options) (*Engine, error) {
	if !filepath.IsAbs(opts.Root) || !filepath.IsAbs(opts.Dir) {
		return nil, errors.New("indexer: Root and Dir must be absolute")
	}
	if rel, err := filepath.Rel(opts.Root, opts.Dir); err == nil && !strings.HasPrefix(rel, "..") {
		return nil, errors.New("indexer: index directory must be outside the workspace root")
	}
	if opts.Workers <= 0 {
		opts.Workers = runtime.GOMAXPROCS(0)
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	st, err := store.Open(opts.Dir, opts.Root, ExtractorVersion)
	if err != nil {
		return nil, err
	}
	if st.Recovered != "" {
		opts.Logf("indexer: rebuilding index: %s", st.Recovered)
	}
	return &Engine{opts: opts, st: st, known: map[string]bool{}}, nil
}

// Root returns the workspace root.
func (e *Engine) Root() string { return e.opts.Root }

// Snapshot returns the current index generation. Call Release when done.
func (e *Engine) Snapshot() *store.Snapshot { return e.st.Snapshot() }

// Manifest returns the published segment list.
func (e *Engine) Manifest() store.Manifest { return e.st.Manifest() }

// Close waits for background compaction and releases the index.
func (e *Engine) Close() error {
	e.closed.Store(true)
	e.wg.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.st.Close()
}

// Reconcile lists the whole workspace and indexes every difference from the
// current generation.
func (e *Engine) Reconcile(ctx context.Context) (Stats, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	start := time.Now()
	var st Stats
	listing, err := scan.List(ctx, e.opts.Root)
	if err != nil {
		return st, fmt.Errorf("list workspace: %w", err)
	}
	st.List = time.Since(start)

	modules := map[string]string{}
	for _, m := range listing.Modules {
		if mp := scan.ModulePath(filepath.Join(e.opts.Root, filepath.FromSlash(m))); mp != "" {
			modules[path.Dir(m)] = mp
		}
	}
	firstPass := e.modules == nil
	modulesChanged := !firstPass && !sameMap(modules, e.modules)
	e.modules = modules

	known := make(map[string]bool, len(listing.Sources))
	for _, p := range listing.Sources {
		known[p] = true
	}
	e.known = known

	sn := e.st.Snapshot()
	defer sn.Release()
	if firstPass {
		// After a restart, compare stored package paths with the current
		// module layout instead of assuming it changed.
		modulesChanged = e.packagesStale(sn)
	}
	full := sn.Generation() == 0 || (modulesChanged && sn.NumFiles() > 0)
	var deleted []string
	for _, p := range sn.Paths() {
		if !known[p] {
			deleted = append(deleted, p)
		}
	}
	return e.index(ctx, sn, listing.Sources, deleted, full, start, st)
}

// Update indexes the given changed paths (absolute or root-relative). Paths
// the indexer does not handle are ignored, except that module files and
// directories trigger a full Reconcile.
func (e *Engine) Update(ctx context.Context, paths []string) (Stats, error) {
	e.mu.Lock()
	candidates, reconcile := e.classify(paths)
	if reconcile || e.modules == nil {
		e.mu.Unlock()
		return e.Reconcile(ctx)
	}
	defer e.mu.Unlock()
	start := time.Now()
	var st Stats
	sn := e.st.Snapshot()
	defer sn.Release()

	var present, deleted, unknown []string
	seen := map[string]bool{}
	for _, rel := range candidates {
		if seen[rel] {
			continue
		}
		seen[rel] = true
		if _, err := os.Lstat(filepath.Join(e.opts.Root, filepath.FromSlash(rel))); err != nil {
			if _, ok := sn.Lookup(rel); ok {
				deleted = append(deleted, rel)
			}
			delete(e.known, rel)
			continue
		}
		if e.known[rel] {
			present = append(present, rel)
		} else {
			unknown = append(unknown, rel)
		}
	}
	if len(unknown) > 0 {
		ignored := scan.Ignored(ctx, e.opts.Root, unknown)
		for _, rel := range unknown {
			if !ignored[rel] {
				e.known[rel] = true
				present = append(present, rel)
			}
		}
	}
	return e.index(ctx, sn, present, deleted, false, start, st)
}

// classify maps changed paths to indexable candidates and reports whether a
// full Reconcile is required instead. Callers hold e.mu.
func (e *Engine) classify(paths []string) (candidates []string, reconcile bool) {
	for _, p := range paths {
		rel, ok := e.rel(p)
		if !ok {
			continue
		}
		switch {
		case scan.Indexable(rel):
			candidates = append(candidates, rel)
		case scan.IsModuleFile(rel) || path.Base(rel) == ".gitignore":
			return nil, true
		case !strings.Contains(path.Base(rel), "."):
			// Likely a directory: created (files may predate the watch) or
			// removed (children vanish without events). Re-list.
			info, err := os.Stat(filepath.Join(e.opts.Root, filepath.FromSlash(rel)))
			if (err == nil && info.IsDir()) || e.hasChildren(rel) {
				return nil, true
			}
		}
	}
	return candidates, false
}

func (e *Engine) hasChildren(dir string) bool {
	prefix := dir + "/"
	for p := range e.known {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func (e *Engine) rel(p string) (string, bool) {
	if filepath.IsAbs(p) {
		r, err := filepath.Rel(e.opts.Root, p)
		if err != nil || r == "." || strings.HasPrefix(r, "..") {
			return "", false
		}
		p = r
	}
	p = filepath.ToSlash(filepath.Clean(p))
	return p, p != "." && !strings.HasPrefix(p, "../")
}

type job struct {
	rel  string
	info os.FileInfo
	out  *facts.File
}

// index stats candidates, extracts the changed ones in parallel and publishes
// one segment: a new base when full, otherwise an overlay.
func (e *Engine) index(ctx context.Context, sn *store.Snapshot, candidates, deleted []string, full bool, start time.Time, st Stats) (Stats, error) {
	t := time.Now()
	st.Scanned = len(candidates)
	jobs := make([]job, 0, len(candidates))
	for _, rel := range candidates {
		info, err := os.Stat(filepath.Join(e.opts.Root, filepath.FromSlash(rel)))
		if err != nil || !info.Mode().IsRegular() {
			if _, ok := sn.Lookup(rel); ok {
				deleted = append(deleted, rel)
			}
			continue
		}
		if !full {
			if m, ok := sn.Meta(rel); ok && m.Size == info.Size() && m.MtimeNS == info.ModTime().UnixNano() {
				continue
			}
		}
		jobs = append(jobs, job{rel: rel, info: info})
	}
	st.Stat = time.Since(t)

	t = time.Now()
	if err := e.extractAll(ctx, jobs); err != nil {
		return st, err
	}
	st.Parse = time.Since(t)
	st.Parsed = len(jobs)

	files := make([]*facts.File, 0, len(jobs)+len(deleted))
	for i := range jobs {
		files = append(files, jobs[i].out)
	}
	if full {
		// A base must hold every live file, including unchanged ones when the
		// rebuild was forced (full implies every candidate was extracted).
		st.Full = true
	} else {
		for _, rel := range deleted {
			files = append(files, &facts.File{Path: rel, Deleted: true})
		}
		st.Deleted = len(deleted)
	}

	t = time.Now()
	if err := e.st.Publish(files, full); err != nil {
		return st, fmt.Errorf("publish index: %w", err)
	}
	st.Publish = time.Since(t)
	st.Generation = e.st.Manifest().Generation
	st.Total = time.Since(start)
	e.maybeCompact()
	return st, nil
}

func (e *Engine) extractAll(ctx context.Context, jobs []job) error {
	if len(jobs) == 0 {
		return nil
	}
	workers := min(e.opts.Workers, len(jobs))
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(jobs) || ctx.Err() != nil {
					return
				}
				jobs[i].out = e.extract(jobs[i].rel, jobs[i].info)
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}

func (e *Engine) extract(rel string, info os.FileInfo) *facts.File {
	f := &facts.File{
		Path: rel, Lang: golang.Lang, Package: e.importPath(rel),
		Size: info.Size(), MtimeNS: info.ModTime().UnixNano(),
	}
	if info.Size() > maxFileBytes {
		f.ParseErr = "file too large to index"
		return f
	}
	src, err := os.ReadFile(filepath.Join(e.opts.Root, filepath.FromSlash(rel)))
	if err != nil {
		f.ParseErr = err.Error()
		return f
	}
	f.Hash = xxhash.Sum64(src)
	golang.Extract(f, src)
	return f
}

// importPath maps a file to its package import path using the nearest
// enclosing go.mod; files outside any module use their directory.
func (e *Engine) importPath(rel string) string {
	dir := path.Dir(rel)
	for d := dir; ; d = path.Dir(d) {
		if mp, ok := e.modules[d]; ok {
			if d == dir {
				return mp
			}
			sub := dir
			if d != "." {
				sub = strings.TrimPrefix(dir, d+"/")
			}
			return mp + "/" + sub
		}
		if d == "." || d == "/" {
			return dir
		}
	}
}

func (e *Engine) maybeCompact() {
	overlays, ob, bb := e.st.OverlayStats()
	if overlays < maxOverlays && (overlays < minOverlaysForSize || ob*maxOverlayFraction <= bb) {
		return
	}
	if e.closed.Load() || !e.compacting.CompareAndSwap(false, true) {
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer e.compacting.Store(false)
		if err := e.st.Compact(); err != nil {
			e.opts.Logf("indexer: compaction: %v", err)
		}
	}()
}

func (e *Engine) packagesStale(sn *store.Snapshot) bool {
	for _, p := range sn.Paths() {
		if m, ok := sn.Meta(p); ok && m.Package != e.importPath(p) {
			return true
		}
	}
	return false
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
