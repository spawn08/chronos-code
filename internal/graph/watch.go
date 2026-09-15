package graph

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceWindow batches bursts of file events (e.g. an editor's save-then-
// touch sequence, or a git checkout touching many files) into one reindex.
const debounceWindow = 300 * time.Millisecond

// Watcher reconciles bursts of source/build-input and directory changes.
type Watcher struct {
	ix       *Indexer
	fsw      *fsnotify.Watcher
	cancel   context.CancelFunc
	done     chan struct{}
	closeErr error
}

// Watch starts a background watcher for ix.Root. Call Close to stop it.
func Watch(ctx context.Context, ix *Indexer) (*Watcher, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fs watcher: %w", err)
	}
	if err := addDirsRecursive(ctx, fsw, ix.Root); err != nil {
		fsw.Close()
		return nil, fmt.Errorf("watch %s: %w", ix.Root, err)
	}

	wctx, cancel := context.WithCancel(ctx)
	w := &Watcher{ix: ix, fsw: fsw, cancel: cancel, done: make(chan struct{})}
	go w.loop(wctx)
	return w, nil
}

// Close stops the watcher and releases its resources.
func (w *Watcher) Close() error {
	w.cancel()
	<-w.done
	return w.closeErr
}

func (w *Watcher) loop(ctx context.Context) {
	defer close(w.done)
	defer func() { w.closeErr = w.fsw.Close() }()

	pending := make(map[string]struct{})
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	timerC := func() <-chan time.Time {
		if timer == nil {
			return nil
		}
		return timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					if skipGraphDir(info.Name()) && info.Name() != ".chronos" {
						continue
					}
					if err := addDirsRecursive(ctx, w.fsw, ev.Name); err != nil && ctx.Err() == nil {
						log.Printf("graph watcher: add directory: %v", err)
					}
				}
			}
			// Removed/renamed directories no longer have stat information.
			// Reconcile them too, including pre-populated directory creates.
			if !graphExtension(ev.Name) && !graphConfigFile(ev.Name) && ev.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			pending[ev.Name] = struct{}{}
			if timer == nil {
				timer = time.NewTimer(debounceWindow)
			} else {
				timer.Reset(debounceWindow)
			}
		case <-timerC():
			timer = nil
			paths := make([]string, 0, len(pending))
			for p := range pending {
				paths = append(paths, p)
				delete(pending, p)
			}
			w.reindex(ctx, paths)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			log.Printf("graph watcher: %v", err)
		}
	}
}

// reindex performs one pre-load scan per burst. Selecting just one event per
// directory could choose an unchanged sibling and suppress the actual edit.
func (w *Watcher) reindex(ctx context.Context, paths []string) {
	if len(paths) == 0 || ctx.Err() != nil {
		return
	}
	if _, err := w.ix.IndexAll(ctx); err != nil && ctx.Err() == nil {
		log.Printf("graph watcher: reindex: %v", err)
	}
}

func addDirsRecursive(ctx context.Context, fsw *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && skipGraphDir(d.Name()) && d.Name() != ".chronos" {
			return filepath.SkipDir
		}
		return fsw.Add(path)
	})
}
