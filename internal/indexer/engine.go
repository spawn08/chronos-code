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
	"slices"
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
const ExtractorVersion = "go-syntax-3"

// Tunables.
const (
	maxOverlays        = 16
	maxOverlayFraction = 4 // overlay bytes > base bytes / maxOverlayFraction
	minOverlaysForSize = 4
	maxOverlayBytes    = 16 << 20
	maxFileBytes       = 4 << 20
	buildChunk         = 1024  // files extracted per streaming step
	maxTouched         = 4096  // Touched paths kept before forcing a full listing
	maxGitChanges      = 50000 // beyond this, a git reconcile lists instead
	defaultProgressive = 20000 // first builds larger than this go progressive
	maxWorkingSet      = 5000
)

// Options configures an Engine.
type Options struct {
	Root    string // absolute workspace root
	Dir     string // absolute index directory, outside Root
	Workers int    // parse workers; 0 = GOMAXPROCS
	Logf    func(format string, args ...any)

	// ShardBytes sizes base shards (estimated fact bytes); 0 = default.
	ShardBytes int
	// ProgressiveFiles: a first build over more files than this publishes
	// the working set first. 0 = default; negative disables.
	ProgressiveFiles int
	// Focus is a root-relative directory whose files join the working set.
	Focus string
	// WatchBackend selects the change source: "" or "auto", "fsnotify",
	// "watchman" or "poll". See watch.go.
	WatchBackend string
	// FsnotifyMaxFiles is the largest index auto mode watches with fsnotify
	// (which needs a descriptor per file on macOS). 0 = default.
	FsnotifyMaxFiles int
	// PollInterval is the git polling period; 0 = default.
	PollInterval time.Duration
}

// Stats describes one indexing pass.
type Stats struct {
	Generation uint64
	Mode       string // "update", "git" (reconciled from git), "list" (full listing), "build" (new base)
	Full       bool   // published a new base
	Scanned    int    // candidate paths examined
	Parsed     int    // files read and extracted
	Reused     int    // files whose content hash matched: facts reused, not parsed
	Deleted    int    // tombstones written
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

	compacting atomic.Bool
	wg         sync.WaitGroup
	closed     atomic.Bool

	busy       atomic.Int32                // indexing passes in progress
	pending    atomic.Int64                // changed paths the watcher has not applied yet
	lastPass   atomic.Int64                // unix nanoseconds of the last completed pass
	watcher    atomic.Pointer[Watcher]     // set by Watch, for Sync
	readyOnce  sync.Once                   // closes ready after the first reconcile
	ready      chan struct{}               // closed once the index reflects the workspace
	lastErr    atomic.Pointer[errorHolder] // last pass error, nil after a success
	reconciled atomic.Bool
	lastMode   atomic.Value // string
}

type errorHolder struct{ err error }

// Status describes how current the index is.
type Status struct {
	Generation uint64
	Files      int
	Shards     int
	Overlays   int
	Pending    int       // changed paths seen but not yet indexed
	Busy       bool      // an indexing pass is running
	Reconciled bool      // a reconcile completed in this process
	Complete   bool      // false while a progressive first build is running
	Mode       string    // how the last reconcile found changes: "git" or "list"
	Backend    string    // active watch backend, if watching
	LastPass   time.Time // zero before the first pass
	LastError  error     // error of the last pass, if it failed
}

// Status reports the current generation and freshness.
func (e *Engine) Status() Status {
	sn := e.st.Snapshot()
	defer sn.Release()
	st := Status{
		Generation: sn.Generation(), Files: sn.NumFiles(), Shards: sn.NumShards(), Overlays: sn.NumSegments() - sn.NumShards(),
		Pending: int(e.pending.Load()), Busy: e.busy.Load() > 0, Reconciled: e.reconciled.Load(),
		Complete: sn.Generation() == 0 || e.st.Meta().Complete,
	}
	st.Mode, _ = e.lastMode.Load().(string)
	if w := e.watcher.Load(); w != nil {
		st.Backend = w.backend.name()
	}
	if ns := e.lastPass.Load(); ns > 0 {
		st.LastPass = time.Unix(0, ns)
	}
	if h := e.lastErr.Load(); h != nil {
		st.LastError = h.err
	}
	return st
}

// Ready is closed after the first successful Reconcile in this process.
func (e *Engine) Ready() <-chan struct{} { return e.ready }

// Sync applies changes the watcher has seen but not yet indexed, and returns
// once they are visible. Without a watcher it returns immediately.
func (e *Engine) Sync(ctx context.Context) error {
	if w := e.watcher.Load(); w != nil {
		return w.Flush(ctx)
	}
	return nil
}

func (e *Engine) finishPass(err error) {
	e.lastPass.Store(time.Now().UnixNano())
	if err != nil {
		e.lastErr.Store(&errorHolder{err})
		return
	}
	e.lastErr.Store(nil)
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
	st.ShardBytes = opts.ShardBytes
	if st.Recovered != "" {
		opts.Logf("indexer: rebuilding index: %s", st.Recovered)
	}
	return &Engine{opts: opts, st: st, ready: make(chan struct{})}, nil
}

// Root returns the workspace root.
func (e *Engine) Root() string { return e.opts.Root }

// Snapshot returns the current index generation. Call Release when done.
func (e *Engine) Snapshot() *store.Snapshot { return e.st.Snapshot() }

// Manifest returns the published manifest.
func (e *Engine) Manifest() store.Manifest { return e.st.Manifest() }

// Close waits for background compaction and releases the index.
func (e *Engine) Close() error {
	e.closed.Store(true)
	e.wg.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.st.Close()
}

// Import replaces the index with a prebuilt one (for example published by
// CI for a commit). Reconcile afterwards indexes only what differs from the
// prebuilt index's commit.
func (e *Engine) Import(src string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.st.Import(src); err != nil {
		return err
	}
	e.modules = nil
	return nil
}

// Reconcile brings the index up to date with the workspace. With a complete
// index reconciled at a known git commit it asks git what changed (the diff
// from that commit, the working tree's changes and the paths touched since)
// and indexes only those. Otherwise it lists the whole workspace.
func (e *Engine) Reconcile(ctx context.Context) (st Stats, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.busy.Add(1)
	defer func() {
		e.busy.Add(-1)
		e.finishPass(err)
		if err == nil {
			e.lastMode.Store(st.Mode)
			e.reconciled.Store(true)
			e.readyOnce.Do(func() { close(e.ready) })
		}
	}()
	start := time.Now()
	sn := e.st.Snapshot()
	defer sn.Release()
	meta := e.st.Meta()
	if sn.Generation() > 0 && meta.Complete && meta.Commit != "" && !meta.TouchedOverflow && meta.Modules != nil {
		if st, ok, err := e.gitReconcile(ctx, sn, meta, start); ok || err != nil {
			return st, err
		}
	}
	return e.listReconcile(ctx, sn, meta, start)
}

// gitReconcile indexes what git reports as changed. ok=false means git
// cannot answer (no repository, unknown commit, module or ignore changes,
// too many changes) and the caller lists instead.
func (e *Engine) gitReconcile(ctx context.Context, sn *store.Snapshot, meta store.Meta, start time.Time) (Stats, bool, error) {
	st := Stats{Mode: "git"}
	head, err := gitHead(ctx, e.opts.Root)
	if err != nil {
		return st, false, nil
	}
	changed := map[string]bool{}
	if head != meta.Commit {
		diff, err := gitPaths(ctx, e.opts.Root, "diff", "--name-only", "-z", "--no-renames", "--relative", meta.Commit, head)
		if err != nil {
			return st, false, nil // commit unknown here (rebased away, shallow clone)
		}
		for _, p := range diff {
			changed[p] = true
		}
	}
	dirty, err := gitDirty(ctx, e.opts.Root)
	if err != nil {
		return st, false, nil
	}
	for _, p := range dirty {
		changed[p] = true
	}
	for _, p := range meta.Touched {
		changed[p] = true
	}
	if len(changed) > maxGitChanges {
		return st, false, nil
	}
	var candidates, deleted []string
	for p := range changed {
		switch {
		case scan.IsModuleFile(p) || path.Base(p) == ".gitignore":
			return st, false, nil
		case !scan.Indexable(p):
		case exists(e.opts.Root, p):
			candidates = append(candidates, p)
		default:
			if _, ok := sn.Lookup(p); ok {
				deleted = append(deleted, p)
			}
		}
	}
	e.modules = meta.Modules
	st.List = time.Since(start)
	slices.Sort(candidates)
	slices.Sort(deleted)
	st, err = e.index(ctx, sn, candidates, deleted, start, st)
	if err != nil {
		return st, true, err
	}
	meta.Commit, meta.Touched = head, indexable(dirty)
	return st, true, e.st.SetMeta(meta)
}

// listReconcile lists the whole workspace and indexes every difference.
func (e *Engine) listReconcile(ctx context.Context, sn *store.Snapshot, meta store.Meta, start time.Time) (Stats, error) {
	st := Stats{Mode: "list"}
	head, _ := gitHead(ctx, e.opts.Root)
	var dirty []string
	if head != "" {
		dirty, _ = gitDirty(ctx, e.opts.Root)
	}
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
	e.modules = modules
	sources := slices.Compact(slices.Sorted(slices.Values(listing.Sources)))
	finish := func(st Stats) (Stats, error) {
		meta := store.Meta{Complete: true, Modules: modules}
		if head != "" {
			meta.Commit, meta.Touched = head, indexable(dirty)
		}
		return st, e.st.SetMeta(meta)
	}
	if sn.Generation() == 0 || !meta.Complete || !sameMap(modules, meta.Modules) {
		if sn.Generation() == 0 && e.progressive(len(sources)) {
			e.publishWorkingSet(ctx, sources, dirty)
		}
		st, err := e.build(ctx, sources, start, st)
		if err != nil {
			return st, err
		}
		return finish(st)
	}
	known := make(map[string]bool, len(sources))
	for _, p := range sources {
		known[p] = true
	}
	var deleted []string
	for _, p := range sn.Paths() {
		if !known[p] {
			deleted = append(deleted, p)
		}
	}
	st, err = e.index(ctx, sn, sources, deleted, start, st)
	if err != nil {
		return st, err
	}
	return finish(st)
}

func (e *Engine) progressive(n int) bool {
	limit := e.opts.ProgressiveFiles
	if limit == 0 {
		limit = defaultProgressive
	}
	return limit > 0 && n > limit
}

// publishWorkingSet indexes the files an agent is most likely to ask about
// first — the focus directory, files changed in recent commits and the
// working tree's changes — and publishes them before the full build, so
// queries answer within seconds on a large repository. Failures only lose
// the head start.
func (e *Engine) publishWorkingSet(ctx context.Context, sources, dirty []string) {
	listed := make(map[string]bool, len(sources))
	for _, p := range sources {
		listed[p] = true
	}
	set := map[string]bool{}
	add := func(p string) {
		if len(set) < maxWorkingSet && listed[p] {
			set[p] = true
		}
	}
	for _, p := range dirty {
		add(p)
	}
	if recent, err := gitPaths(ctx, e.opts.Root, "log", "-n", "50", "-z", "--name-only", "--format=", "--relative"); err == nil {
		for _, p := range recent {
			add(p)
		}
	}
	if focus := strings.Trim(filepath.ToSlash(e.opts.Focus), "/"); focus != "" && focus != "." {
		for _, p := range sources {
			if strings.HasPrefix(p, focus+"/") {
				add(p)
			}
		}
	}
	if len(set) == 0 {
		return
	}
	jobs := make([]job, 0, len(set))
	for p := range set {
		if info, err := os.Stat(filepath.Join(e.opts.Root, filepath.FromSlash(p))); err == nil && info.Mode().IsRegular() {
			jobs = append(jobs, job{rel: p, info: info})
		}
	}
	if e.extractAll(ctx, jobs, nil) != nil {
		return
	}
	files := make([]*facts.File, len(jobs))
	for i := range jobs {
		files[i] = jobs[i].out
	}
	e.st.StageMeta(store.Meta{Complete: false})
	if err := e.st.Publish(files, false); err != nil {
		e.opts.Logf("indexer: working set: %v", err)
	}
}

// build streams every source into a new base, a chunk at a time, so memory
// holds at most one chunk and one shard of facts.
func (e *Engine) build(ctx context.Context, sources []string, start time.Time, st Stats) (Stats, error) {
	st.Mode, st.Full, st.Scanned = "build", true, len(sources)
	w := e.st.NewBase()
	for lo := 0; lo < len(sources); lo += buildChunk {
		t := time.Now()
		chunk := sources[lo:min(lo+buildChunk, len(sources))]
		jobs := make([]job, 0, len(chunk))
		for _, rel := range chunk {
			info, err := os.Stat(filepath.Join(e.opts.Root, filepath.FromSlash(rel)))
			if err == nil && info.Mode().IsRegular() {
				jobs = append(jobs, job{rel: rel, info: info})
			}
		}
		st.Stat += time.Since(t)
		t = time.Now()
		if err := e.extractAll(ctx, jobs, nil); err != nil {
			w.Abort()
			return st, err
		}
		st.Parse += time.Since(t)
		st.Parsed += len(jobs)
		t = time.Now()
		for i := range jobs {
			if err := w.Add(jobs[i].out); err != nil {
				w.Abort()
				return st, err
			}
		}
		st.Publish += time.Since(t)
	}
	t := time.Now()
	if err := w.Commit(); err != nil {
		return st, fmt.Errorf("publish index: %w", err)
	}
	st.Publish += time.Since(t)
	st.Generation = e.st.Manifest().Generation
	st.Total = time.Since(start)
	return st, nil
}

// Update indexes the given changed paths (absolute or root-relative). Paths
// the indexer does not handle are ignored, except that module files, ignore
// files and directories trigger a Reconcile.
func (e *Engine) Update(ctx context.Context, paths []string) (st Stats, err error) {
	e.mu.Lock()
	sn := e.st.Snapshot()
	candidates, reconcile := e.classify(sn, paths)
	if reconcile || e.modules == nil {
		sn.Release()
		e.mu.Unlock()
		return e.Reconcile(ctx)
	}
	defer e.mu.Unlock()
	defer sn.Release()
	e.busy.Add(1)
	defer func() {
		e.busy.Add(-1)
		e.finishPass(err)
	}()
	start := time.Now()
	st.Mode = "update"
	var present, deleted, unknown []string
	seen := map[string]bool{}
	for _, rel := range candidates {
		if seen[rel] {
			continue
		}
		seen[rel] = true
		_, indexed := sn.Lookup(rel)
		if !exists(e.opts.Root, rel) {
			if indexed {
				deleted = append(deleted, rel)
			}
			continue
		}
		if indexed {
			present = append(present, rel)
		} else {
			unknown = append(unknown, rel)
		}
	}
	if len(unknown) > 0 {
		ignored := scan.Ignored(ctx, e.opts.Root, unknown)
		for _, rel := range unknown {
			if !ignored[rel] {
				present = append(present, rel)
			}
		}
	}
	if len(present)+len(deleted) > 0 {
		meta := e.st.Meta()
		meta.Touched = appendTouched(meta.Touched, append(slices.Clone(present), deleted...))
		if len(meta.Touched) > maxTouched {
			meta.Touched, meta.TouchedOverflow = nil, true
		}
		e.st.StageMeta(meta)
	}
	return e.index(ctx, sn, present, deleted, start, st)
}

func appendTouched(touched, paths []string) []string {
	have := make(map[string]bool, len(touched))
	for _, p := range touched {
		have[p] = true
	}
	for _, p := range paths {
		if !have[p] {
			have[p] = true
			touched = append(touched, p)
		}
	}
	return touched
}

// classify maps changed paths to indexable candidates and reports whether a
// Reconcile is required instead. Callers hold e.mu.
func (e *Engine) classify(sn *store.Snapshot, paths []string) (candidates []string, reconcile bool) {
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
			if (err == nil && info.IsDir()) || sn.HasPrefix(rel+"/") {
				return nil, true
			}
		}
	}
	return candidates, false
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

// index stats candidates, extracts the changed ones in parallel and
// publishes them as one overlay. A candidate whose size and mtime match the
// index is skipped; one whose content hash matches is re-recorded with the
// new stat from its stored facts, without parsing.
func (e *Engine) index(ctx context.Context, sn *store.Snapshot, candidates, deleted []string, start time.Time, st Stats) (Stats, error) {
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
		if m, ok := sn.Meta(rel); ok && m.Size == info.Size() && m.MtimeNS == info.ModTime().UnixNano() && m.Package == e.importPath(rel) {
			continue
		}
		jobs = append(jobs, job{rel: rel, info: info})
	}
	st.Stat = time.Since(t)

	t = time.Now()
	var reused atomic.Int64
	reuse := func(rel string, hash uint64) *facts.File {
		m, ok := sn.Meta(rel)
		if !ok || m.Hash != hash || m.ParseErr != "" || m.Package != e.importPath(rel) {
			return nil
		}
		ref, _ := sn.Lookup(rel)
		reused.Add(1)
		return sn.Segment(int(ref.Seg)).File(int(ref.File))
	}
	if err := e.extractAll(ctx, jobs, reuse); err != nil {
		return st, err
	}
	st.Parse = time.Since(t)
	st.Reused = int(reused.Load())
	st.Parsed = len(jobs) - st.Reused

	files := make([]*facts.File, 0, len(jobs)+len(deleted))
	for i := range jobs {
		files = append(files, jobs[i].out)
	}
	for _, rel := range deleted {
		files = append(files, &facts.File{Path: rel, Deleted: true})
	}
	st.Deleted = len(deleted)

	t = time.Now()
	if err := e.st.Publish(files, false); err != nil {
		return st, fmt.Errorf("publish index: %w", err)
	}
	st.Publish = time.Since(t)
	st.Generation = e.st.Manifest().Generation
	st.Total = time.Since(start)
	e.maybeCompact()
	return st, nil
}

func (e *Engine) extractAll(ctx context.Context, jobs []job, reuse func(string, uint64) *facts.File) error {
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
				jobs[i].out = e.extract(jobs[i].rel, jobs[i].info, reuse)
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}

func (e *Engine) extract(rel string, info os.FileInfo, reuse func(string, uint64) *facts.File) *facts.File {
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
	if reuse != nil {
		if old := reuse(rel, f.Hash); old != nil {
			old.Size, old.MtimeNS, old.Package = f.Size, f.MtimeNS, f.Package
			return old
		}
	}
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
	if overlays < maxOverlays && ob < maxOverlayBytes && (overlays < minOverlaysForSize || ob*maxOverlayFraction <= bb) {
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

func exists(root, rel string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

func indexable(paths []string) []string {
	var out []string
	for _, p := range paths {
		if scan.Indexable(p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > maxTouched {
		out = out[:maxTouched]
	}
	return out
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
