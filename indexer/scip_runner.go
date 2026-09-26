package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spawn08/chronos-code/indexer/precise"
	"github.com/spawn08/chronos-code/indexer/scip"
	"github.com/spawn08/chronos-code/indexer/store"
)

// The SCIP tier (M10) is the precise tier for languages other than Go. It
// imports SCIP indexes (package scip) in the background into a store of
// the same kind as the Go tier's, and queries use a fact only while its
// file's hash matches, exactly as for go/types facts.
//
// Index files are checked every SCIPPoll and imported (all together) when
// one changes; <root>/index.scip is imported when present. A source with
// a command is also rerun, one at a time, once indexing has been quiet for
// SCIPQuiet after files of the languages its index covers changed. A run
// records the hashes of the files that did not change while it ran, so its
// documents are gated exactly; documents of an index built elsewhere are
// checked against the files (scip.Import). Edits never wait for an import
// or a run and never cancel one.

const (
	defaultSCIPQuiet   = 30 * time.Second
	defaultSCIPPoll    = 5 * time.Second
	defaultSCIPTimeout = 30 * time.Minute
	scipImportQuiet    = time.Second // after indexing, before an import it asked for
	scipStateFile      = "state.json"
	scipOutputTail     = 4 << 10
)

// SCIPSource is one SCIP index the SCIP tier imports.
type SCIPSource struct {
	// Index is the .scip file, absolute or relative to Root.
	Index string
	// Dir is the root-relative directory the index's document paths are
	// relative to when its project root is not inside Root, and Command's
	// working directory. "" is the root.
	Dir string
	// Command, if set, is run with sh -c in the background to (re)write
	// Index. Empty imports Index as it is.
	Command string
}

// SCIP tier states.
const (
	SCIPOff       = "off"
	SCIPWaiting   = "waiting"   // work waits for indexing to be quiet
	SCIPRunning   = "running"   // a source's command is running
	SCIPImporting = "importing" // indexes are being imported
	SCIPReady     = "ready"     // every index present is imported
)

// SCIPStatus describes the SCIP tier.
type SCIPStatus struct {
	State      string
	Indexes    int           // index files present at the last import
	Docs       int           // documents imported
	Sites      int           // reference sites imported
	Rejected   int           // documents left out as not describing the current file
	LastImport time.Duration // duration of the last import
	LastError  string
}

type scipSource struct {
	SCIPSource
	index string // absolute
	dir   string // root-relative, "." for the root
}

// stamp identifies a version of an index file.
type stamp struct {
	Size    int64 `json:"size"`
	MtimeNS int64 `json:"mtime_ns"`
}

// scipState is persisted next to the facts, so a reopened index with
// current facts imports nothing.
type scipState struct {
	Imported map[string]stamp    `json:"imported"` // index -> version last imported
	Runs     map[string]*scipRun `json:"runs"`     // index -> its command's last run
}

// scipRun records what a command's last run observed.
type scipRun struct {
	Index    stamp             `json:"index"`    // the index file it wrote
	Observed map[string]uint64 `json:"observed"` // files unchanged during the run
	Exts     []string          `json:"exts"`     // extensions of the files its index describes
}

type scipRunner struct {
	e       *Engine
	store   *precise.Store
	dir     string
	sources []scipSource
	quiet   time.Duration
	poll    time.Duration
	timeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	jobMu  sync.Mutex // one import or run at a time

	mu      sync.Mutex
	state   scipState
	check   bool // an import or a run may be due
	retry   bool // the last import left out files not indexed yet
	timer   *time.Timer
	running bool
	closed  bool
	started bool // a reconcile completed: snapshots describe the workspace
	status  atomic.Pointer[SCIPStatus]
}

func newSCIPRunner(e *Engine, st *precise.Store, dir string) *scipRunner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &scipRunner{
		e: e, store: st, dir: dir, quiet: e.opts.SCIPQuiet, poll: e.opts.SCIPPoll, timeout: e.opts.SCIPTimeout,
		ctx: ctx, cancel: cancel, check: true,
	}
	if r.quiet <= 0 {
		r.quiet = defaultSCIPQuiet
	}
	if r.poll <= 0 {
		r.poll = defaultSCIPPoll
	}
	if r.timeout <= 0 {
		r.timeout = defaultSCIPTimeout
	}
	root := e.opts.Root
	seen := map[string]bool{}
	for _, s := range e.opts.SCIPSources {
		src := scipSource{SCIPSource: s, index: s.Index, dir: path.Clean(filepath.ToSlash(s.Dir))}
		if src.dir == "" {
			src.dir = "."
		}
		if !filepath.IsAbs(src.index) {
			src.index = filepath.Join(root, filepath.FromSlash(s.Dir), filepath.FromSlash(s.Index))
		}
		if !seen[src.index] {
			seen[src.index] = true
			r.sources = append(r.sources, src)
		}
	}
	if def := filepath.Join(root, "index.scip"); !seen[def] {
		r.sources = append(r.sources, scipSource{index: def, dir: "."})
	}
	r.state = r.loadState()
	r.status.Store(&SCIPStatus{State: SCIPWaiting})
	r.wg.Add(1)
	go r.pollLoop()
	return r
}

func (r *scipRunner) setStatus(f func(*SCIPStatus)) {
	s := *r.status.Load()
	f(&s)
	r.status.Store(&s)
}

func (r *scipRunner) loadState() scipState {
	st := scipState{Imported: map[string]stamp{}, Runs: map[string]*scipRun{}}
	if data, err := os.ReadFile(filepath.Join(r.dir, scipStateFile)); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if st.Imported == nil {
		st.Imported = map[string]stamp{}
	}
	if st.Runs == nil {
		st.Runs = map[string]*scipRun{}
	}
	return st
}

func (r *scipRunner) saveState() error {
	r.mu.Lock()
	data, err := json.Marshal(r.state)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	name := filepath.Join(r.dir, scipStateFile)
	if err := os.WriteFile(name+".tmp", data, 0o644); err != nil {
		return err
	}
	return os.Rename(name+".tmp", name)
}

func stampOf(name string) (stamp, bool) {
	fi, err := os.Stat(name)
	if err != nil || !fi.Mode().IsRegular() {
		return stamp{}, false
	}
	return stamp{Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano()}, true
}

// note records that an indexing pass completed (full: a reconcile, after
// which snapshots describe the workspace and work may start) and
// schedules a check once indexing is quiet.
func (r *scipRunner) note(paths []string, full bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.started = r.started || full
	if len(paths) > 0 || full || r.retry {
		r.check = true
	}
	if r.check {
		r.scheduleLocked(scipImportQuiet)
	}
}

func (r *scipRunner) scheduleLocked(d time.Duration) {
	if r.closed || !r.started {
		return
	}
	if r.timer == nil {
		r.timer = time.AfterFunc(d, r.fire)
	} else {
		r.timer.Reset(d)
	}
}

// pollLoop notices index files written by other tools.
func (r *scipRunner) pollLoop() {
	defer r.wg.Done()
	t := time.NewTicker(r.poll)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			if r.indexesChanged() {
				r.mu.Lock()
				r.check = true
				r.scheduleLocked(0)
				r.mu.Unlock()
			}
		}
	}
}

// indexesChanged reports whether an index file differs from the version
// last imported (written, rewritten or removed).
func (r *scipRunner) indexesChanged() bool {
	r.mu.Lock()
	imported := r.state.Imported
	r.mu.Unlock()
	for _, s := range r.sources {
		st, ok := stampOf(s.index)
		old, had := imported[s.index]
		if ok != had || ok && st != old {
			return true
		}
	}
	return false
}

// fire runs the due work, unless indexing is busy (it retries later).
func (r *scipRunner) fire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.running || !r.check {
		return
	}
	if r.e.busy.Load() > 0 || r.e.pending.Load() > 0 {
		r.timer.Reset(scipImportQuiet)
		return
	}
	r.running, r.check = true, false
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		_ = r.run(r.ctx, false)
	}()
}

// run runs the commands that are due, then imports if an index changed or
// the last import left files for later. force runs commands without
// waiting for the quiet period and always imports.
func (r *scipRunner) run(ctx context.Context, force bool) error {
	r.jobMu.Lock()
	defer r.jobMu.Unlock()
	var next time.Duration
	defer func() {
		r.mu.Lock()
		r.running = false
		if next > 0 {
			r.check = true
			r.scheduleLocked(next)
		}
		r.mu.Unlock()
	}()
	var errs []error
	for _, s := range r.sources {
		if s.Command == "" {
			continue
		}
		due, wait := r.commandDue(s, force)
		if !due {
			if wait > 0 && (next == 0 || wait < next) {
				next = wait
			}
			continue
		}
		r.setStatus(func(st *SCIPStatus) { st.State = SCIPRunning })
		if err := r.runCommand(ctx, s); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
		}
	}
	r.mu.Lock()
	retry := r.retry
	r.mu.Unlock()
	if force || retry || r.indexesChanged() {
		if err := r.importAll(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
		}
	}
	err := errors.Join(errs...)
	r.setStatus(func(st *SCIPStatus) {
		st.State = SCIPReady
		if next > 0 {
			st.State = SCIPWaiting
		}
		st.LastError = ""
		if err != nil {
			st.LastError = err.Error()
		}
	})
	return err
}

// commandDue reports whether s's command should run now: its index is
// missing, or files of the languages its index covers changed since its
// last run started (or during it), and indexing has been quiet for the
// quiet period; otherwise wait is how long until it may be due.
func (r *scipRunner) commandDue(s scipSource, force bool) (due bool, wait time.Duration) {
	r.mu.Lock()
	last := r.state.Runs[s.index]
	r.mu.Unlock()
	if _, ok := stampOf(s.index); ok && last != nil {
		sn := r.e.st.Snapshot()
		changed := coveredChanged(sn, s.dir, last)
		sn.Release()
		if !changed {
			return false, 0
		}
	}
	if ns := r.e.lastPass.Load(); ns > 0 && !force {
		if idle := time.Since(time.Unix(0, ns)); idle < r.quiet {
			return false, r.quiet - idle
		}
	}
	return true, 0
}

// coveredChanged reports whether a file under dir with one of the run's
// extensions was added, removed or changed since the run observed it.
func coveredChanged(sn *store.Snapshot, dir string, last *scipRun) bool {
	exts := map[string]bool{}
	for _, x := range last.Exts {
		exts[x] = true
	}
	n := 0
	for _, p := range sn.Paths() {
		if !under(p, dir) || !exts[path.Ext(p)] {
			continue
		}
		m, ok := sn.Meta(p)
		if !ok {
			continue
		}
		n++
		if h, ok := last.Observed[p]; !ok || h != m.Hash {
			return true
		}
	}
	covered := 0
	for p := range last.Observed {
		if exts[path.Ext(p)] {
			covered++
		}
	}
	return n != covered
}

func under(p, dir string) bool {
	return dir == "." || p == dir || strings.HasPrefix(p, dir+"/")
}

// runCommand runs s's command and records the files that did not change
// while it ran.
func (r *scipRunner) runCommand(ctx context.Context, s scipSource) error {
	before := r.hashesUnder(s.dir)
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", s.Command)
	cmd.Dir = filepath.Join(r.e.opts.Root, filepath.FromSlash(s.dir))
	var out tailBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	r.e.opts.Logf("indexer: scip: running %q", s.Command)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scip: %q: %w: %s", s.Command, err, strings.TrimSpace(out.String()))
	}
	written, ok := stampOf(s.index)
	if !ok {
		return fmt.Errorf("scip: %q did not write %s", s.Command, s.index)
	}
	// Wait for the watcher to apply what changed during the run, so the
	// comparison sees it (bounded: a busy workspace compares what it has).
	syncCtx, syncCancel := context.WithTimeout(ctx, 10*time.Second)
	_ = r.e.Sync(syncCtx)
	syncCancel()
	// A file is observed unchanged if its hash is the same and it was not
	// modified during the run (an edit reverted meanwhile may have been
	// read by the indexer).
	end := time.Now()
	sn := r.e.st.Snapshot()
	observed := map[string]uint64{}
	for p, h := range before {
		m, ok := sn.Meta(p)
		if ok && m.Hash == h && (m.MtimeNS < start.UnixNano() || m.MtimeNS > end.UnixNano()) {
			observed[p] = h
		}
	}
	sn.Release()
	exts := r.extsOf(s)
	r.mu.Lock()
	r.state.Runs[s.index] = &scipRun{Index: written, Observed: observed, Exts: exts}
	r.mu.Unlock()
	return r.saveState()
}

// extsOf returns the extensions of the files s's index describes.
func (r *scipRunner) extsOf(s scipSource) []string {
	seen := map[string]bool{}
	var out []string
	_ = scip.ReadFile(s.index, nil, func(d scip.Document) error {
		if x := path.Ext(d.Path); x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
		return nil
	})
	return out
}

func (r *scipRunner) hashesUnder(dir string) map[string]uint64 {
	sn := r.e.st.Snapshot()
	defer sn.Release()
	out := map[string]uint64{}
	for _, p := range sn.Paths() {
		if under(p, dir) {
			if m, ok := sn.Meta(p); ok && m.Hash != 0 {
				out[p] = m.Hash
			}
		}
	}
	return out
}

// importAll imports every index present and replaces the stored facts.
func (r *scipRunner) importAll(ctx context.Context) error {
	r.setStatus(func(st *SCIPStatus) { st.State = SCIPImporting })
	start := time.Now()
	sn := r.e.st.Snapshot()
	defer sn.Release()
	r.mu.Lock()
	runs := r.state.Runs
	r.mu.Unlock()
	var srcs []scip.Source
	stamps := map[string]stamp{}
	for _, s := range r.sources {
		st, ok := stampOf(s.index)
		if !ok {
			continue
		}
		stamps[s.index] = st
		src := scip.Source{Index: s.index, Dir: s.dir}
		if last := runs[s.index]; last != nil && s.Command != "" && last.Index == st {
			src.Observed = last.Observed // written by the run we watched
		}
		srcs = append(srcs, src)
	}
	res, err := scip.Import(ctx, scip.Options{
		Root: r.e.opts.Root,
		Indexed: func(p string) (uint64, bool) {
			m, ok := sn.Meta(p)
			return m.Hash, ok
		},
		Skip: func(p string) bool { return strings.HasSuffix(p, ".go") }, // the Go tier's
	}, srcs)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if res == nil {
		return err
	}
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	keep := map[string]bool{}
	for _, d := range res.Dirs {
		keep[d.Path] = true
	}
	var gone []string
	for _, d := range r.store.All() {
		if !keep[d.Path] {
			gone = append(gone, d.Path)
		}
	}
	if len(gone) > 0 {
		r.store.Delete(gone)
	}
	if err := r.store.Put(res.Dirs); err != nil {
		errs = append(errs, err)
	}
	retry := false
	for _, why := range res.Rejected {
		if why == "not indexed yet" {
			retry = true
		}
	}
	r.mu.Lock()
	r.state.Imported = stamps
	r.retry = retry
	r.mu.Unlock()
	if err := r.saveState(); err != nil {
		errs = append(errs, err)
	}
	r.setStatus(func(st *SCIPStatus) {
		st.Indexes, st.Docs, st.Sites, st.Rejected, st.LastImport = len(srcs), res.Docs, res.Sites, len(res.Rejected), time.Since(start)
	})
	if len(res.Rejected) > 0 {
		r.e.opts.Logf("indexer: scip: imported %d documents; %d left out as not describing the current file", res.Docs, len(res.Rejected))
	}
	return errors.Join(errs...)
}

func (r *scipRunner) close() {
	r.mu.Lock()
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	r.cancel()
	r.wg.Wait()
}

// tailBuffer keeps the last scipOutputTail bytes written to it.
type tailBuffer struct{ b bytes.Buffer }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b.Write(p)
	if extra := t.b.Len() - scipOutputTail; extra > 0 {
		t.b.Next(extra)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return t.b.String() }

// SCIP returns the SCIP tier's store, or nil when it is off.
func (e *Engine) SCIP() *precise.Store {
	if e.scip == nil {
		return nil
	}
	return e.scip.store
}

// SCIPStatus reports the SCIP tier's state.
func (e *Engine) SCIPStatus() SCIPStatus {
	if e.scip == nil {
		return SCIPStatus{State: SCIPOff}
	}
	return *e.scip.status.Load()
}

// RunSCIP runs the due SCIP work now (commands whose files changed or
// whose index is missing, regardless of the quiet period, and an import
// of every index present) and returns when it is stored (for tools and
// tests).
func (e *Engine) RunSCIP(ctx context.Context) error {
	if e.scip == nil {
		return errors.New("indexer: the SCIP tier is off")
	}
	r := e.scip
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("indexer: closed")
	}
	r.running, r.check = true, false
	r.wg.Add(1) // Close waits for it
	r.mu.Unlock()
	defer r.wg.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(r.ctx, cancel) // Close cancels it
	defer stop()
	return r.run(ctx, true)
}
