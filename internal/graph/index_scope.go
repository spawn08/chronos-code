package graph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"

	"github.com/spawn08/chronos-code/internal/indexer"
	"github.com/spawn08/chronos-code/internal/indexer/query"
	"github.com/spawn08/chronos-code/internal/indexer/store"
)

const (
	// maxExtraRoots bounds how many non-primary workspace roots (plan
	// worktrees, request overrides) keep an open index at once.
	maxExtraRoots = 4
	// nonGoRefreshInterval throttles the tree-sitter tier's freshness scan.
	nonGoRefreshInterval = time.Second
)

// IndexScopeOptions configures an IndexScope.
type IndexScopeOptions struct {
	Root    string // primary workspace root
	DataDir string // per-project data directory; indexes live in DataDir/index
	// GraphDB is the SQLite store of the tree-sitter tier (non-Go files).
	// It is used only in builds with the treesitter tag; "" disables it.
	GraphDB      string
	IndexOnStart bool // reconcile the primary root in the background at once
	Watch        bool // keep indexes current from filesystem events
	Logf         func(format string, args ...any)
}

// IndexScope serves the graph tools from the chronos indexer. Unlike
// RequestScope it never type-checks and never rebuilds on the request path:
// each call applies changes the watcher has already seen, then answers from
// one immutable snapshot and reports how fresh that snapshot is.
type IndexScope struct {
	opts   IndexScopeOptions
	root   string
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
	roots  map[string]*indexRoot
	lru    []string // extra roots, least recently used first
	nonGo  *nonGoTier
	live   *liveBackend
}

type indexRoot struct {
	root    string
	engine  *indexer.Engine
	cache   *query.Cache
	private string // session-private index dir, removed on close

	startOnce sync.Once
	started   chan struct{}
	startErr  error
	launched  bool // guarded by IndexScope.mu
	watcher   *indexer.Watcher
	refs      int
}

// NewIndexScope opens the primary root's index. It returns at once; with
// IndexOnStart the first reconcile runs in the background, and tool calls
// are answered from the stored index meanwhile (they wait only when no index
// exists yet).
func NewIndexScope(ctx context.Context, opts IndexScopeOptions) (*IndexScope, error) {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	root, err := canonicalGraphRoot(opts.Root)
	if err != nil {
		return nil, err
	}
	if opts.DataDir == "" {
		return nil, errors.New("graph index: data directory is required")
	}
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &IndexScope{opts: opts, root: root, ctx: sctx, cancel: cancel, roots: map[string]*indexRoot{}}
	s.live = &liveBackend{s: s}
	primary, err := s.openRoot(root, filepath.Join(opts.DataDir, "index", "v1"))
	if err != nil {
		cancel()
		return nil, err
	}
	s.roots[root] = primary
	if opts.GraphDB != "" && len(SupportedTreeSitterExtensions()) > 0 {
		if st, err := OpenStore(opts.GraphDB); err != nil {
			opts.Logf("code graph: non-Go tier unavailable: %v", err)
		} else {
			s.nonGo = &nonGoTier{store: st, ix: NewIndexer(st, root)}
		}
	}
	if opts.IndexOnStart {
		s.startInBackground(primary)
		if s.nonGo != nil {
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.nonGo.refresh(s.ctx, s.opts.Logf)
			}()
		}
	}
	return s, nil
}

// Root returns the canonical primary workspace root.
func (s *IndexScope) Root() string { return s.root }

// Engine returns the primary root's engine (for status reporting).
func (s *IndexScope) Engine() *indexer.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.roots[s.root]; r != nil {
		return r.engine
	}
	return nil
}

// openRoot opens the index for root in dir. A directory inside the root
// (a legacy in-checkout data dir) moves to the user cache; an index locked by
// another live session is replaced by a session-private one.
func (s *IndexScope) openRoot(root, dir string) (*indexRoot, error) {
	if isPathWithin(root, dir) {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("graph index: no index directory outside %s: %w", root, err)
		}
		dir = filepath.Join(base, "chronos-code", "index", rootKey(root), "v1")
	}
	r := &indexRoot{root: root, cache: query.NewCache(), started: make(chan struct{})}
	eng, err := indexer.Open(indexer.Options{Root: root, Dir: dir, Logf: s.opts.Logf})
	if errors.Is(err, store.ErrLocked) {
		base := filepath.Join(filepath.Dir(dir), "sessions")
		if err := os.MkdirAll(base, 0o755); err != nil {
			return nil, fmt.Errorf("graph index: %w", err)
		}
		private, err := os.MkdirTemp(base, "index-")
		if err != nil {
			return nil, fmt.Errorf("graph index: %w", err)
		}
		s.opts.Logf("code graph: index at %s is in use by another session; using a private index", dir)
		r.private = private
		eng, err = indexer.Open(indexer.Options{Root: root, Dir: private, Logf: s.opts.Logf})
		if err != nil {
			_ = os.RemoveAll(private)
			return nil, fmt.Errorf("graph index: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("graph index: %w", err)
	}
	r.engine = eng
	return r, nil
}

func rootKey(root string) string { return fmt.Sprintf("%016x", xxhash.Sum64String(root)) }

// startInBackground launches the root's start once. It does nothing after
// Close, so the WaitGroup is never added to while Close waits on it.
func (s *IndexScope) startInBackground(r *indexRoot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || r.launched {
		return
	}
	r.launched = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.start(r)
	}()
}

// start runs the root's first reconcile and starts its watcher, once.
func (s *IndexScope) start(r *indexRoot) {
	r.startOnce.Do(func() {
		defer close(r.started)
		began := time.Now()
		st, err := r.engine.Reconcile(s.ctx)
		if err != nil {
			r.startErr = err
			if s.ctx.Err() == nil {
				s.opts.Logf("code graph: index %s: %v", r.root, err)
			}
			return
		}
		s.opts.Logf("code graph: indexed %s (generation %d, %d files parsed) in %s", r.root, st.Generation, st.Parsed, time.Since(began).Round(time.Millisecond))
		if s.opts.Watch && s.ctx.Err() == nil {
			w, err := r.engine.Watch(s.ctx, nil)
			if err != nil {
				s.opts.Logf("code graph: watch %s: %v", r.root, err)
				return
			}
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				_ = w.Close()
				return
			}
			r.watcher = w
			s.mu.Unlock()
		}
	})
}

// acquire returns the open index for root, opening an extra root on first
// use. The caller must call s.release(r).
func (s *IndexScope) acquire(root string) (*indexRoot, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("code graph is closed")
	}
	r := s.roots[root]
	if r != nil {
		r.refs++
		s.touch(root)
		s.mu.Unlock()
		return r, nil
	}
	s.mu.Unlock()
	opened, err := s.openRoot(root, filepath.Join(s.opts.DataDir, "index", "roots", rootKey(root), "v1"))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.closeRoot(opened)
		return nil, errors.New("code graph is closed")
	}
	if existing := s.roots[root]; existing != nil {
		// Lost a race with another caller opening the same root.
		existing.refs++
		s.touch(root)
		s.mu.Unlock()
		s.closeRoot(opened)
		return existing, nil
	}
	opened.refs = 1
	s.roots[root] = opened
	s.touch(root)
	evict := s.evictLocked()
	s.mu.Unlock()
	for _, e := range evict {
		s.closeRoot(e)
	}
	return opened, nil
}

func (s *IndexScope) touch(root string) {
	if root == s.root {
		return
	}
	s.lru = slices.DeleteFunc(s.lru, func(r string) bool { return r == root })
	s.lru = append(s.lru, root)
}

func (s *IndexScope) evictLocked() []*indexRoot {
	var out []*indexRoot
	for i := 0; len(s.lru)-len(out) > maxExtraRoots && i < len(s.lru); i++ {
		r := s.roots[s.lru[i]]
		if r == nil || r.refs > 0 {
			continue
		}
		delete(s.roots, r.root)
		out = append(out, r)
	}
	for _, r := range out {
		s.lru = slices.DeleteFunc(s.lru, func(x string) bool { return x == r.root })
	}
	return out
}

func (s *IndexScope) release(r *indexRoot) {
	s.mu.Lock()
	r.refs--
	s.mu.Unlock()
}

func (s *IndexScope) closeRoot(r *indexRoot) {
	// Never started: prevent a later start. Starting: wait for it to finish.
	r.startOnce.Do(func() { close(r.started) })
	s.mu.Lock()
	w := r.watcher
	r.watcher = nil
	s.mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
	_ = r.engine.Close()
	if r.private != "" {
		_ = os.RemoveAll(r.private)
	}
}

// backend returns a Backend over the current snapshot of the workspace root
// the request targets, and its release function.
func (s *IndexScope) backend(ctx context.Context) (Backend, string, func(), error) {
	root, err := canonicalGraphRoot(builtins.WorkspaceRoot(ctx, s.opts.Root))
	if err != nil {
		return nil, s.opts.Root, nil, err
	}
	r, err := s.acquire(root)
	if err != nil {
		return nil, root, nil, err
	}
	if err := s.ready(ctx, r); err != nil {
		s.release(r)
		return nil, root, nil, err
	}
	if err := r.engine.Sync(ctx); err != nil {
		s.release(r)
		return nil, root, nil, err
	}
	st := r.engine.Status()
	sn := r.engine.Snapshot()
	b := &indexBackend{view: query.NewView(sn, r.cache), root: root, report: reportFrom(st)}
	var out Backend = b
	if root == s.root && s.nonGo != nil {
		s.nonGo.refresh(ctx, s.opts.Logf)
		out = &mergedBackend{primary: b, extra: s.nonGo.store}
	}
	return out, root, func() { sn.Release(); s.release(r) }, nil
}

// ready makes sure the root has been reconciled at least once in this
// process, or at least has a stored index to answer from. Without either it
// waits for the first build (bounded by ctx).
func (s *IndexScope) ready(ctx context.Context, r *indexRoot) error {
	select {
	case <-r.started:
		if r.startErr != nil && r.engine.Status().Generation == 0 {
			return fmt.Errorf("index unavailable: %w", r.startErr)
		}
		return nil
	default:
	}
	s.startInBackground(r)
	if r.engine.Status().Generation > 0 {
		return nil // answer from the stored index while reconciling
	}
	select {
	case <-r.started:
		if r.startErr != nil {
			return fmt.Errorf("index unavailable: %w", r.startErr)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func reportFrom(st indexer.Status) IndexReport {
	r := IndexReport{
		Mode: "syntactic", Generation: st.Generation, Files: st.Files, Pending: st.Pending,
		Building: !st.Reconciled, Relations: query.NameMatched,
	}
	if st.LastError != nil {
		r.Error = st.LastError.Error()
	}
	r.UpToDate = st.Reconciled && st.Pending == 0 && st.LastError == nil
	return r
}

// Tools returns the graph navigation tools served from the index.
func (s *IndexScope) Tools() []*tool.Definition { return s.wrap(Tools(nil, s.opts.Root), Tools) }

// ImpactTools returns impact_analysis, test_map and co_change served from
// the index.
func (s *IndexScope) ImpactTools() []*tool.Definition {
	return s.wrap(ImpactTools(nil, s.opts.Root), ImpactTools)
}

func (s *IndexScope) wrap(templates []*tool.Definition, build func(Backend, string) []*tool.Definition) []*tool.Definition {
	out := make([]*tool.Definition, 0, len(templates))
	for _, template := range templates {
		def := *template
		name := template.Name
		def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			b, root, release, err := s.backend(ctx)
			if err != nil {
				return graphUnavailable(root, err), nil
			}
			defer release()
			for _, d := range build(b, root) {
				if d.Name == name {
					return d.Handler(ctx, args)
				}
			}
			return nil, fmt.Errorf("%s: tool unavailable", name)
		}
		out = append(out, &def)
	}
	return out
}

// Live returns a Backend that answers every call from the then-current
// snapshot of the request's workspace root. It suits callers outside tool
// handlers (prefetch, status lines) that make independent lookups.
func (s *IndexScope) Live() Backend { return s.live }

// Close stops watchers and background work and closes every index.
func (s *IndexScope) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
	s.mu.Lock()
	roots := make([]*indexRoot, 0, len(s.roots))
	for _, r := range s.roots {
		roots = append(roots, r)
	}
	s.roots = map[string]*indexRoot{}
	s.mu.Unlock()
	for _, r := range roots {
		s.closeRoot(r)
	}
	if s.nonGo != nil {
		return s.nonGo.store.Close()
	}
	return nil
}

// nonGoTier keeps the SQLite store's tree-sitter facts current.
type nonGoTier struct {
	store *Store
	ix    *Indexer
	mu    sync.Mutex
	last  time.Time
}

func (t *nonGoTier) refresh(ctx context.Context, logf func(string, ...any)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Since(t.last) < nonGoRefreshInterval {
		return
	}
	if err := t.ix.RefreshNonGo(ctx); err != nil && ctx.Err() == nil {
		logf("code graph: non-Go tier: %v", err)
	}
	t.last = time.Now()
}

// liveBackend forwards each call to a fresh snapshot backend.
type liveBackend struct{ s *IndexScope }

var _ Backend = (*liveBackend)(nil)

func live[T any](ctx context.Context, l *liveBackend, fn func(Backend) (T, error)) (T, error) {
	b, _, release, err := l.s.backend(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	defer release()
	return fn(b)
}

func (l *liveBackend) FindSymbols(ctx context.Context, name, kind string) ([]Symbol, error) {
	return live(ctx, l, func(b Backend) ([]Symbol, error) { return b.FindSymbols(ctx, name, kind) })
}

func (l *liveBackend) FindSymbolsFuzzy(ctx context.Context, substr string) ([]Symbol, error) {
	return live(ctx, l, func(b Backend) ([]Symbol, error) { return b.FindSymbolsFuzzy(ctx, substr) })
}

func (l *liveBackend) Search(ctx context.Context, q string, topK int) ([]SearchResult, error) {
	return live(ctx, l, func(b Backend) ([]SearchResult, error) { return b.Search(ctx, q, topK) })
}

func (l *liveBackend) SymbolsInPackage(ctx context.Context, pkg string) ([]Symbol, error) {
	return live(ctx, l, func(b Backend) ([]Symbol, error) { return b.SymbolsInPackage(ctx, pkg) })
}

func (l *liveBackend) FilesInPackage(ctx context.Context, pkg string) ([]FileRecord, error) {
	return live(ctx, l, func(b Backend) ([]FileRecord, error) { return b.FilesInPackage(ctx, pkg) })
}

func (l *liveBackend) SymbolsInFile(ctx context.Context, file string) ([]Symbol, error) {
	return live(ctx, l, func(b Backend) ([]Symbol, error) { return b.SymbolsInFile(ctx, file) })
}

func (l *liveBackend) CallersOf(ctx context.Context, name string) ([]string, error) {
	return live(ctx, l, func(b Backend) ([]string, error) { return b.CallersOf(ctx, name) })
}

func (l *liveBackend) CallersOfMany(ctx context.Context, names []string) (map[string][]string, error) {
	return live(ctx, l, func(b Backend) (map[string][]string, error) { return b.CallersOfMany(ctx, names) })
}

func (l *liveBackend) CallerSymbols(ctx context.Context, name string, limit int) ([]Symbol, error) {
	return live(ctx, l, func(b Backend) ([]Symbol, error) { return b.CallerSymbols(ctx, name, limit) })
}

func (l *liveBackend) SymbolsByQualified(ctx context.Context, names []string) (map[string][]Symbol, error) {
	return live(ctx, l, func(b Backend) (map[string][]Symbol, error) { return b.SymbolsByQualified(ctx, names) })
}

func (l *liveBackend) CalleesOf(ctx context.Context, name string) ([]string, error) {
	return live(ctx, l, func(b Backend) ([]string, error) { return b.CalleesOf(ctx, name) })
}

func (l *liveBackend) ImplementationsOf(ctx context.Context, iface string) ([]string, error) {
	return live(ctx, l, func(b Backend) ([]string, error) { return b.ImplementationsOf(ctx, iface) })
}

func (l *liveBackend) ImportsOf(ctx context.Context, pkg string) ([]string, error) {
	return live(ctx, l, func(b Backend) ([]string, error) { return b.ImportsOf(ctx, pkg) })
}

func (l *liveBackend) ImportersOf(ctx context.Context, pkg string) ([]string, error) {
	return live(ctx, l, func(b Backend) ([]string, error) { return b.ImportersOf(ctx, pkg) })
}

func (l *liveBackend) PackageImports(ctx context.Context, pkg string) (string, error) {
	return live(ctx, l, func(b Backend) (string, error) { return b.PackageImports(ctx, pkg) })
}

func (l *liveBackend) Packages(ctx context.Context) ([]string, error) {
	return live(ctx, l, func(b Backend) ([]string, error) { return b.Packages(ctx) })
}

func (l *liveBackend) Stats(ctx context.Context) (Stats, error) {
	return live(ctx, l, func(b Backend) (Stats, error) { return b.Stats(ctx) })
}

func (l *liveBackend) FileHash(ctx context.Context, path string) (string, error) {
	return live(ctx, l, func(b Backend) (string, error) { return b.FileHash(ctx, path) })
}

func (l *liveBackend) FileProvenance(ctx context.Context, paths ...string) (string, int64, error) {
	type prov struct {
		hash  string
		mtime int64
	}
	p, err := live(ctx, l, func(b Backend) (prov, error) {
		h, m, err := b.FileProvenance(ctx, paths...)
		return prov{h, m}, err
	})
	return p.hash, p.mtime, err
}

// describeReport renders a one-line freshness summary for notes.
func describeReport(r IndexReport) string {
	var parts []string
	switch {
	case r.Building:
		parts = append(parts, "the index is still being built, so recent changes may be missing")
	case r.Pending > 0:
		parts = append(parts, fmt.Sprintf("%d changed file(s) not yet indexed", r.Pending))
	case r.Error != "":
		parts = append(parts, "the last index update failed: "+r.Error)
	default:
		parts = append(parts, "index up to date")
	}
	parts = append(parts, fmt.Sprintf("generation %d, %d files", r.Generation, r.Files))
	return strings.Join(parts, "; ")
}
