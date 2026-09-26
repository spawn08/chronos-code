package indexer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spawn08/chronos-code/indexer/precise"
	"github.com/spawn08/chronos-code/indexer/store"
)

// The type-checked tier (M4) runs in the background: once indexing has
// been quiet for PreciseQuiet, the Go package directories changed since
// the last load (on the first run, every directory whose facts are missing
// or stale) are type-checked with go/packages, one load at a time, and
// their facts stored (package precise). Edits do not wait for it and do
// not cancel it: facts of files edited during a load fail the hash check
// at query time, and those directories load again after the next quiet
// period. A missing toolchain or a package that does not type-check only
// leaves syntactic answers in place.

const (
	defaultPreciseQuiet = time.Second
	maxPreciseDirs      = 256 // directories per load; more load the whole module
)

// Precise tier states.
const (
	PreciseOff         = "off"
	PreciseWaiting     = "waiting"     // changes wait for the quiet period
	PreciseLoading     = "loading"     // a load is running
	PreciseReady       = "ready"       // every change so far is loaded (or failed)
	PreciseUnavailable = "unavailable" // no go toolchain
)

// PreciseStatus describes the type-checked tier.
type PreciseStatus struct {
	State     string
	Dirs      int           // directories with facts from the last completed load
	Failed    int           // directories the last load could not type-check
	LastLoad  time.Duration // duration of the last completed load
	LastError string
}

type preciseRunner struct {
	e     *Engine
	store *precise.Store
	quiet time.Duration
	env   []string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	loadMu sync.Mutex // one load at a time

	mu      sync.Mutex
	dirty   map[string]bool // changed directories (root-relative)
	scan    bool            // compare every Go directory with the store
	timer   *time.Timer
	running bool
	closed  bool
	status  atomic.Pointer[PreciseStatus]
}

func newPreciseRunner(e *Engine, st *precise.Store) *preciseRunner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &preciseRunner{e: e, store: st, quiet: e.opts.PreciseQuiet, env: e.opts.PreciseEnv, ctx: ctx, cancel: cancel, dirty: map[string]bool{}, scan: true}
	if r.quiet <= 0 {
		r.quiet = defaultPreciseQuiet
	}
	r.status.Store(&PreciseStatus{State: PreciseWaiting})
	return r
}

func (r *preciseRunner) setStatus(f func(*PreciseStatus)) {
	s := *r.status.Load()
	f(&s)
	r.status.Store(&s)
}

// note records the paths an indexing pass changed and restarts the quiet
// period. full asks for a comparison of every directory with the store.
func (r *preciseRunner) note(paths []string, full bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	for _, p := range paths {
		if strings.HasSuffix(p, ".go") {
			r.dirty[path.Dir(p)] = true
		}
	}
	r.scan = r.scan || full
	if len(r.dirty) == 0 && !r.scan {
		return
	}
	if st := r.status.Load().State; st != PreciseLoading && st != PreciseUnavailable {
		r.setStatus(func(s *PreciseStatus) { s.State = PreciseWaiting })
	}
	if r.timer == nil {
		r.timer = time.AfterFunc(r.quiet, r.fire)
	} else {
		r.timer.Reset(r.quiet)
	}
}

// fire starts a load after the quiet period, unless indexing is busy or a
// load is running (which reschedules when it ends).
func (r *preciseRunner) fire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.running {
		return
	}
	if r.e.busy.Load() > 0 || r.e.pending.Load() > 0 {
		r.timer.Reset(r.quiet)
		return
	}
	r.running = true
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		_ = r.run(r.ctx)
	}()
}

// take removes the pending work.
func (r *preciseRunner) take() (dirs []string, scan bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for d := range r.dirty {
		dirs = append(dirs, d)
	}
	scan = r.scan
	r.dirty, r.scan = map[string]bool{}, false
	return dirs, scan
}

// requeue puts work back after a failed or cancelled load.
func (r *preciseRunner) requeue(dirs []string, scan bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range dirs {
		r.dirty[d] = true
	}
	r.scan = r.scan || scan
}

// run performs one load of the pending work.
func (r *preciseRunner) run(ctx context.Context) error {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		again := !r.closed && (len(r.dirty) > 0 || r.scan) && r.status.Load().State != PreciseUnavailable
		if again && r.timer != nil {
			r.timer.Reset(r.quiet)
		}
		r.mu.Unlock()
	}()
	if err := precise.Available(); err != nil {
		r.take()
		r.setStatus(func(s *PreciseStatus) { s.State, s.LastError = PreciseUnavailable, err.Error() })
		return err
	}
	dirs, scan := r.take()
	r.setStatus(func(s *PreciseStatus) { s.State = PreciseLoading })
	start := time.Now()
	loaded, failed, err := r.load(ctx, dirs, scan)
	if err != nil {
		if ctx.Err() == nil {
			r.requeue(dirs, scan)
		}
		r.setStatus(func(s *PreciseStatus) { s.State, s.LastError = PreciseReady, err.Error() })
		return err
	}
	r.setStatus(func(s *PreciseStatus) {
		s.State, s.Dirs, s.Failed, s.LastLoad, s.LastError = PreciseReady, loaded, failed, time.Since(start), ""
	})
	r.mu.Lock()
	if len(r.dirty) > 0 || r.scan {
		r.setStatus(func(s *PreciseStatus) { s.State = PreciseWaiting })
	}
	r.mu.Unlock()
	return nil
}

// load type-checks dirs (plus, with scan, every Go directory whose facts
// are missing or stale) module by module, and stores the facts.
func (r *preciseRunner) load(ctx context.Context, dirs []string, scan bool) (loaded, failed int, err error) {
	sn := r.e.st.Snapshot()
	defer sn.Release()
	modules := r.e.st.Meta().Modules
	want := map[string]bool{}
	for _, d := range dirs {
		want[d] = true
	}
	var goFiles map[string]map[string]uint64 // dir -> path -> hash, when needed
	listGo := func() map[string]map[string]uint64 {
		if goFiles == nil {
			goFiles = goFilesByDir(sn)
		}
		return goFiles
	}
	if scan {
		files := listGo()
		for dir, fs := range files {
			if stale(r.store.Get(dir), fs) {
				want[dir] = true
			}
		}
		var gone []string
		for _, d := range r.store.All() {
			if _, ok := files[d.Path]; !ok {
				gone = append(gone, d.Path)
			}
		}
		if len(gone) > 0 {
			r.store.Delete(gone)
		}
	}
	// Group by the nearest enclosing go.mod.
	byModule := map[string][]string{}
	for d := range want {
		if mod, ok := moduleOf(modules, d); ok {
			byModule[mod] = append(byModule[mod], d)
		}
	}
	mods := make([]string, 0, len(byModule))
	for m := range byModule {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	var errs []error
	for _, mod := range mods {
		ds := byModule[mod]
		sort.Strings(ds)
		var patterns []string // nil loads the whole module
		if len(ds) <= maxPreciseDirs {
			patterns = ds
		}
		res, err := precise.Load(ctx, r.e.opts.Root, mod, patterns, r.env)
		if err != nil {
			if ctx.Err() != nil {
				return loaded, failed, ctx.Err()
			}
			errs = append(errs, fmt.Errorf("module %s: %w", mod, err))
			continue
		}
		got := map[string]bool{}
		for _, d := range res.Dirs {
			got[d.Path] = true
		}
		// Directories go list did not return (only build-constrained or
		// ignored files) are recorded with their current hashes and no
		// facts, so they are not loaded again until they change.
		var markers []*precise.Dir
		for _, d := range ds {
			if got[d] || res.Failed[d] != "" {
				continue
			}
			if fs, ok := listGo()[d]; ok {
				m := &precise.Dir{Path: d, Files: map[string]precise.File{}}
				for p, h := range fs {
					m.Files[p] = precise.File{Hash: h}
				}
				markers = append(markers, m)
			} else {
				r.store.Delete([]string{d})
			}
		}
		// Files a load skipped (other platforms' build tags) get markers
		// too, so the directory does not look stale.
		for _, d := range res.Dirs {
			if fs, ok := listGo()[d.Path]; ok {
				for p, h := range fs {
					if _, ok := d.Files[p]; !ok {
						d.Files[p] = precise.File{Hash: h}
					}
				}
			}
		}
		if err := r.store.Put(append(res.Dirs, markers...)); err != nil {
			errs = append(errs, err)
		}
		loaded += len(res.Dirs)
		failed += len(res.Failed)
	}
	return loaded, failed, errors.Join(errs...)
}

// stale reports whether a directory's stored facts miss or disagree with
// the indexed Go files.
func stale(d *precise.Dir, files map[string]uint64) bool {
	if d == nil || len(d.Files) != len(files) {
		return true
	}
	for p, h := range files {
		if f, ok := d.Files[p]; !ok || f.Hash != h {
			return true
		}
	}
	return false
}

// goFilesByDir lists the indexed Go files with their hashes, by directory.
// Directories the go command ignores (testdata, vendor, names starting
// with _ or .) are left out.
func goFilesByDir(sn *store.Snapshot) map[string]map[string]uint64 {
	out := map[string]map[string]uint64{}
	for _, p := range sn.Paths() {
		if !strings.HasSuffix(p, ".go") || goIgnored(p) {
			continue
		}
		m, ok := sn.Meta(p)
		if !ok || m.Hash == 0 {
			continue
		}
		d := path.Dir(p)
		if out[d] == nil {
			out[d] = map[string]uint64{}
		}
		out[d][p] = m.Hash
	}
	return out
}

func goIgnored(p string) bool {
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if seg == "testdata" || seg == "vendor" || strings.HasPrefix(seg, "_") || strings.HasPrefix(seg, ".") && seg != "." {
			return true
		}
	}
	base := path.Base(p)
	return strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".")
}

// moduleOf returns the go.mod directory enclosing dir.
func moduleOf(modules map[string]string, dir string) (string, bool) {
	for d := dir; ; d = path.Dir(d) {
		if _, ok := modules[d]; ok {
			return d, true
		}
		if d == "." || d == "/" {
			return "", false
		}
	}
}

func (r *preciseRunner) close() {
	r.mu.Lock()
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	r.cancel()
	r.wg.Wait()
}

// Precise returns the type-checked tier's store, or nil when it is off.
func (e *Engine) Precise() *precise.Store {
	if e.precise == nil {
		return nil
	}
	return e.precise.store
}

// PreciseStatus reports the type-checked tier's state.
func (e *Engine) PreciseStatus() PreciseStatus {
	if e.precise == nil {
		return PreciseStatus{State: PreciseOff}
	}
	return *e.precise.status.Load()
}

// RunPrecise loads the pending precise work now, without waiting for the
// quiet period, and returns when it is stored (for tools and tests).
func (e *Engine) RunPrecise(ctx context.Context) error {
	if e.precise == nil {
		return errors.New("indexer: the precise tier is off")
	}
	r := e.precise
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("indexer: closed")
	}
	r.running = true
	r.mu.Unlock()
	return r.run(ctx)
}
