package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/spawn08/chronos-code/internal/indexer/scan"
)

// Watch timing: flush a quiet burst after watchDebounce, and never hold a
// continuous burst longer than watchMaxDelay. More than watchMaxDirty paths
// in one burst fall back to a Reconcile.
const (
	watchDebounce       = 50 * time.Millisecond
	watchMaxDelay       = 2 * time.Second
	watchMaxDirty       = 4096
	defaultPollInterval = 2 * time.Second
	watchmanInterval    = 200 * time.Millisecond
)

// defaultFsnotifyMaxFiles is the largest index auto mode watches with
// fsnotify. On macOS and the BSDs fsnotify uses kqueue, which holds a
// descriptor for every file in every watched directory; on Linux inotify
// holds one watch per directory.
func defaultFsnotifyMaxFiles() int {
	switch runtime.GOOS {
	case "darwin", "freebsd", "netbsd", "openbsd", "dragonfly":
		return 20000
	}
	return 200000
}

// batch is a set of changed paths from a backend; reconcile means changes
// were lost and the index must be reconciled instead.
type batch struct {
	paths     []string
	reconcile bool
}

// backend is a source of workspace changes.
type backend interface {
	name() string
	// run delivers batches until ctx ends.
	run(ctx context.Context, out chan<- batch)
	// pollNow returns changes immediately, for Flush; ok=false means the
	// backend has nothing cheap to offer and Flush relies on delivered events.
	pollNow(ctx context.Context) (batch, bool)
	close() error
}

// Watcher keeps an Engine up to date from a change backend.
type Watcher struct {
	e       *Engine
	backend backend
	cancel  context.CancelFunc
	done    chan struct{}
	flush   chan chan struct{}
	// onUpdate, if set, is called after every pass.
	onUpdate func(Stats, error)
}

// Watch starts watching the workspace with the backend Options.WatchBackend
// selects (auto: fsnotify for indexes up to FsnotifyMaxFiles files, then
// watchman when installed, then git polling). Call Close to stop. onUpdate
// may be nil.
func (e *Engine) Watch(ctx context.Context, onUpdate func(Stats, error)) (*Watcher, error) {
	b, err := e.pickBackend(ctx)
	if err != nil {
		return nil, err
	}
	wctx, cancel := context.WithCancel(ctx)
	w := &Watcher{e: e, backend: b, cancel: cancel, done: make(chan struct{}), flush: make(chan chan struct{}), onUpdate: onUpdate}
	e.watcher.Store(w)
	events := make(chan batch, 64)
	go b.run(wctx, events)
	go w.loop(wctx, events)
	return w, nil
}

func (e *Engine) pickBackend(ctx context.Context) (backend, error) {
	mode := e.opts.WatchBackend
	if mode == "" || mode == "auto" {
		limit := e.opts.FsnotifyMaxFiles
		if limit <= 0 {
			limit = defaultFsnotifyMaxFiles()
		}
		sn := e.st.Snapshot()
		n := sn.NumFiles()
		sn.Release()
		switch {
		case n <= limit:
			mode = "fsnotify"
		case watchmanAvailable():
			mode = "watchman"
		default:
			if _, err := gitHead(ctx, e.opts.Root); err == nil {
				mode = "poll"
			} else {
				mode = "fsnotify"
				e.opts.Logf("indexer: %d files and neither watchman nor git: watching with fsnotify", n)
			}
		}
	}
	switch mode {
	case "fsnotify":
		return newFsnotifyBackend(ctx, e.opts.Root, e.opts.Logf)
	case "watchman":
		b, err := newWatchmanBackend(ctx, e.opts.Root)
		if err == nil || e.opts.WatchBackend == "watchman" {
			return b, err
		}
		e.opts.Logf("indexer: watchman: %v; polling git instead", err)
		fallthrough
	case "poll":
		interval := e.opts.PollInterval
		if interval <= 0 {
			interval = defaultPollInterval
		}
		return &pollBackend{root: e.opts.Root, interval: interval}, nil
	}
	return nil, fmt.Errorf("indexer: unknown watch backend %q", mode)
}

// Flush applies every change seen so far without waiting for the debounce,
// and returns once it is visible in the engine's snapshots. A backend that
// can poll cheaply (watchman) is polled first.
func (w *Watcher) Flush(ctx context.Context) error {
	if _, cheap := w.backend.(*watchmanBackend); !cheap && w.e.pending.Load() == 0 {
		return nil
	}
	reply := make(chan struct{})
	select {
	case w.flush <- reply:
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Backend returns the name of the active backend.
func (w *Watcher) Backend() string { return w.backend.name() }

// Close stops the watcher and waits for any in-flight pass.
func (w *Watcher) Close() error {
	w.cancel()
	<-w.done
	w.e.watcher.CompareAndSwap(w, nil)
	w.e.pending.Store(0)
	return w.backend.close()
}

func (w *Watcher) loop(ctx context.Context, events <-chan batch) {
	defer close(w.done)
	dirty := map[string]struct{}{}
	overflow := false
	var quiet, deadline *time.Timer
	var quietC, deadlineC <-chan time.Time
	stop := func() {
		if quiet != nil {
			quiet.Stop()
		}
		if deadline != nil {
			deadline.Stop()
		}
		quiet, deadline, quietC, deadlineC = nil, nil, nil, nil
	}
	defer stop()
	add := func(b batch) {
		if b.reconcile {
			overflow = true
		}
		for _, p := range b.paths {
			if len(dirty) >= watchMaxDirty {
				overflow = true
				break
			}
			dirty[p] = struct{}{}
		}
		if len(dirty) > 0 || overflow {
			w.e.pending.Store(int64(max(len(dirty), 1)))
		}
	}
	flush := func() {
		stop()
		paths := make([]string, 0, len(dirty))
		for p := range dirty {
			paths = append(paths, p)
		}
		clear(dirty)
		var st Stats
		var err error
		if overflow {
			st, err = w.e.Reconcile(ctx)
		} else if len(paths) > 0 {
			st, err = w.e.Update(ctx, paths)
		}
		overflow = false
		w.e.pending.Store(0)
		if err != nil && ctx.Err() == nil {
			w.e.opts.Logf("indexer: watch update: %v", err)
		}
		if w.onUpdate != nil && ctx.Err() == nil {
			w.onUpdate(st, err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-events:
			add(b)
			if len(dirty) == 0 && !overflow {
				continue
			}
			if quiet == nil {
				quiet = time.NewTimer(watchDebounce)
				deadline = time.NewTimer(watchMaxDelay)
				quietC, deadlineC = quiet.C, deadline.C
			} else {
				quiet.Reset(watchDebounce)
			}
		case <-quietC:
			flush()
		case <-deadlineC:
			flush()
		case reply := <-w.flush:
			if b, ok := w.backend.pollNow(ctx); ok {
				add(b)
			}
			if len(dirty) > 0 || overflow {
				flush()
			}
			close(reply)
		}
	}
}

// fsnotifyBackend watches every directory with fsnotify.
type fsnotifyBackend struct {
	fsw  *fsnotify.Watcher
	logf func(string, ...any)
}

func newFsnotifyBackend(ctx context.Context, root string, logf func(string, ...any)) (*fsnotifyBackend, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create watcher: %w", err)
	}
	if err := addTree(ctx, fsw, root); err != nil {
		fsw.Close()
		return nil, fmt.Errorf("watch %s: %w", root, err)
	}
	return &fsnotifyBackend{fsw: fsw, logf: logf}, nil
}

func (b *fsnotifyBackend) name() string { return "fsnotify" }

func (b *fsnotifyBackend) run(ctx context.Context, out chan<- batch) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-b.fsw.Events:
			if !ok {
				return
			}
			if ev.Op == fsnotify.Chmod {
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() && !scan.SkipDir(info.Name()) {
					if err := addTree(ctx, b.fsw, ev.Name); err != nil && ctx.Err() == nil {
						b.logf("indexer: watch directory: %v", err)
					}
				}
			}
			send(ctx, out, batch{paths: []string{ev.Name}})
		case err, ok := <-b.fsw.Errors:
			if !ok {
				return
			}
			// Typically an event queue overflow: events were lost.
			b.logf("indexer: watch: %v; reconciling", err)
			send(ctx, out, batch{reconcile: true})
		}
	}
}

func (b *fsnotifyBackend) pollNow(context.Context) (batch, bool) { return batch{}, false }
func (b *fsnotifyBackend) close() error                          { return b.fsw.Close() }

func send(ctx context.Context, out chan<- batch, b batch) {
	select {
	case out <- b:
	case <-ctx.Done():
	}
}

func addTree(ctx context.Context, fsw *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !d.IsDir() {
			return nil
		}
		if p != root && scan.SkipDir(d.Name()) {
			return filepath.SkipDir
		}
		return fsw.Add(p)
	})
}

// pollBackend asks git for working-tree changes on an interval: no
// descriptor per file, and O(changes) work when core.fsmonitor is set. A
// moved HEAD (commit, checkout, pull) triggers a git reconcile. Files that
// became clean since the last poll are reported too, so reverts are seen.
type pollBackend struct {
	root     string
	interval time.Duration
	mu       sync.Mutex
	head     string
	last     map[string]bool
}

func (b *pollBackend) name() string { return "poll" }

func (b *pollBackend) run(ctx context.Context, out chan<- batch) {
	b.poll(ctx) // baseline
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if bt, ok := b.poll(ctx); ok && (bt.reconcile || len(bt.paths) > 0) {
				send(ctx, out, bt)
			}
		}
	}
}

func (b *pollBackend) poll(ctx context.Context) (batch, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	head, err := gitHead(ctx, b.root)
	if err != nil {
		return batch{}, false
	}
	dirty, err := gitDirty(ctx, b.root)
	if err != nil {
		return batch{}, false
	}
	cur := make(map[string]bool, len(dirty))
	for _, p := range dirty {
		cur[p] = true
	}
	var bt batch
	if b.head != "" && head != b.head {
		bt.reconcile = true
	}
	if b.last != nil {
		for p := range cur {
			bt.paths = append(bt.paths, p)
		}
		for p := range b.last {
			if !cur[p] {
				bt.paths = append(bt.paths, p)
			}
		}
	}
	b.head, b.last = head, cur
	return bt, true
}

// pollNow is not offered: a git poll stats the whole index without
// fsmonitor, too slow for every tool call. Changes appear within one
// interval.
func (b *pollBackend) pollNow(context.Context) (batch, bool) { return batch{}, false }
func (b *pollBackend) close() error                          { return nil }

// watchmanBackend queries a running watchman daemon (started on demand by
// the watchman CLI) for files changed since the last clock. Each query costs
// O(changes), so Flush polls it before every tool call.
type watchmanBackend struct {
	watch, relative string
	mu              sync.Mutex
	clock           string
}

func watchmanAvailable() bool {
	_, err := exec.LookPath("watchman")
	return err == nil
}

func watchmanCall(ctx context.Context, req any, resp any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	in, err := json.Marshal(req)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "watchman", "-j", "--no-pretty")
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("watchman: %w", err)
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(out, &e) == nil && e.Error != "" {
		return fmt.Errorf("watchman: %s", e.Error)
	}
	return json.Unmarshal(out, resp)
}

func newWatchmanBackend(ctx context.Context, root string) (*watchmanBackend, error) {
	var wp struct {
		Watch        string `json:"watch"`
		RelativePath string `json:"relative_path"`
	}
	if err := watchmanCall(ctx, []any{"watch-project", root}, &wp); err != nil {
		return nil, err
	}
	var clk struct {
		Clock string `json:"clock"`
	}
	if err := watchmanCall(ctx, []any{"clock", wp.Watch}, &clk); err != nil {
		return nil, err
	}
	if wp.Watch == "" || clk.Clock == "" {
		return nil, fmt.Errorf("watchman: incomplete response for %s", root)
	}
	return &watchmanBackend{watch: wp.Watch, relative: wp.RelativePath, clock: clk.Clock}, nil
}

func (b *watchmanBackend) name() string { return "watchman" }

func (b *watchmanBackend) run(ctx context.Context, out chan<- batch) {
	t := time.NewTicker(watchmanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if bt, ok := b.pollNow(ctx); ok && (bt.reconcile || len(bt.paths) > 0) {
				send(ctx, out, bt)
			}
		}
	}
}

func (b *watchmanBackend) pollNow(ctx context.Context) (batch, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := map[string]any{"since": b.clock, "fields": []string{"name"}, "empty_on_fresh_instance": true}
	if b.relative != "" {
		q["relative_root"] = b.relative
	}
	var resp struct {
		Clock string   `json:"clock"`
		Files []string `json:"files"`
		Fresh bool     `json:"is_fresh_instance"`
	}
	if err := watchmanCall(ctx, []any{"query", b.watch, q}, &resp); err != nil {
		return batch{}, false
	}
	if resp.Clock != "" {
		b.clock = resp.Clock
	}
	return batch{paths: resp.Files, reconcile: resp.Fresh}, true
}

func (b *watchmanBackend) close() error { return nil }
