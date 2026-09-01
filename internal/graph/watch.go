package graph

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceWindow batches bursts of file events (e.g. an editor's save-then-
// touch sequence, or a git checkout touching many files) into one reindex.
const debounceWindow = 300 * time.Millisecond

// Watcher applies incremental reindexes as .go files change under the
// indexer's root, so the graph stays fresh without a full reindex per edit.
type Watcher struct {
	ix     *Indexer
	fsw    *fsnotify.Watcher
	cancel context.CancelFunc
	done   chan struct{}
}

// Watch starts a background watcher for ix.Root. Call Close to stop it.
func Watch(ctx context.Context, ix *Indexer) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fs watcher: %w", err)
	}
	if err := addDirsRecursive(fsw, ix.Root); err != nil {
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
	return w.fsw.Close()
}

func (w *Watcher) loop(ctx context.Context) {
	defer close(w.done)

	pending := make(map[string]struct{})
	var timer *time.Timer
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
			if !strings.HasSuffix(ev.Name, ".go") {
				if ev.Op&fsnotify.Create != 0 {
					if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
						_ = addDirsRecursive(w.fsw, ev.Name)
					}
				}
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

// reindex calls IndexFile once per changed directory. IndexFile itself
// checks each file's content hash before doing any real work, so a save
// event where an editor rewrote a file without changing its bytes (a
// common touch-on-save pattern) is cheap here rather than needing its own
// hash check in this loop.
func (w *Watcher) reindex(ctx context.Context, paths []string) {
	seenDirs := make(map[string]struct{})
	for _, p := range paths {
		dir := filepath.Dir(p)
		if _, ok := seenDirs[dir]; ok {
			continue
		}
		seenDirs[dir] = struct{}{}
		if _, err := w.ix.IndexFile(ctx, p); err != nil {
			log.Printf("graph watcher: reindex %s: %v", dir, err)
		}
	}
}

func addDirsRecursive(fsw *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if name != "." && strings.HasPrefix(name, ".") {
			return filepath.SkipDir
		}
		if name == "vendor" || name == "node_modules" {
			return filepath.SkipDir
		}
		return fsw.Add(path)
	})
}
