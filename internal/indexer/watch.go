package indexer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/spawn08/chronos-code/internal/indexer/scan"
)

// Watch timing: flush a quiet burst after watchDebounce, and never hold a
// continuous burst longer than watchMaxDelay. More than watchMaxDirty paths
// in one burst fall back to a full Reconcile.
const (
	watchDebounce = 50 * time.Millisecond
	watchMaxDelay = 2 * time.Second
	watchMaxDirty = 4096
)

// Watcher keeps an Engine up to date from filesystem events.
type Watcher struct {
	e      *Engine
	fsw    *fsnotify.Watcher
	cancel context.CancelFunc
	done   chan struct{}
	// OnUpdate, if set before events arrive, is called after every pass.
	onUpdate func(Stats, error)
}

// Watch starts watching the workspace. Call Close to stop. onUpdate may be nil.
func (e *Engine) Watch(ctx context.Context, onUpdate func(Stats, error)) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create watcher: %w", err)
	}
	if err := addTree(ctx, fsw, e.opts.Root); err != nil {
		fsw.Close()
		return nil, fmt.Errorf("watch %s: %w", e.opts.Root, err)
	}
	wctx, cancel := context.WithCancel(ctx)
	w := &Watcher{e: e, fsw: fsw, cancel: cancel, done: make(chan struct{}), onUpdate: onUpdate}
	go w.loop(wctx)
	return w, nil
}

// Close stops the watcher and waits for any in-flight pass.
func (w *Watcher) Close() error {
	w.cancel()
	<-w.done
	return w.fsw.Close()
}

func (w *Watcher) loop(ctx context.Context) {
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
		} else {
			st, err = w.e.Update(ctx, paths)
		}
		overflow = false
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
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if ev.Op == fsnotify.Chmod {
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() && !scan.SkipDir(info.Name()) {
					if err := addTree(ctx, w.fsw, ev.Name); err != nil && ctx.Err() == nil {
						w.e.opts.Logf("indexer: watch directory: %v", err)
					}
				}
			}
			if len(dirty) >= watchMaxDirty {
				overflow = true
			} else {
				dirty[ev.Name] = struct{}{}
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
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			// Typically an event queue overflow: events were lost.
			w.e.opts.Logf("indexer: watch: %v; reconciling", err)
			overflow = true
			if quiet == nil {
				quiet = time.NewTimer(watchDebounce)
				deadline = time.NewTimer(watchMaxDelay)
				quietC, deadlineC = quiet.C, deadline.C
			}
		}
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
