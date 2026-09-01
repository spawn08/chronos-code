package projectdocs

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceWindow batches bursts of file events (an editor's save-then-touch
// sequence, a branch switch touching several files at once) into one
// recompute, mirroring internal/graph/watch.go's debounce window.
const debounceWindow = 300 * time.Millisecond

// Watcher watches every directory level between a Bundle's root and cwd for
// changes to any candidateFiles entry, calling onChange (debounced) so the
// caller can reload and re-render.
type Watcher struct {
	fsw    *fsnotify.Watcher
	cancel context.CancelFunc
	done   chan struct{}
}

// Watch starts a background watcher over dirs (typically the directory
// levels a Bundle was loaded from — root down to cwd). onChange is called
// (from the watcher's own goroutine) after a debounceWindow-quiet period
// following any create/write/remove/rename event for one of candidateFiles
// in one of dirs. Call Close to stop it.
func Watch(ctx context.Context, dirs []string, onChange func()) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("projectdocs: create fs watcher: %w", err)
	}
	seen := make(map[string]bool)
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if err := fsw.Add(dir); err != nil {
			fsw.Close()
			return nil, fmt.Errorf("projectdocs: watch %s: %w", dir, err)
		}
	}

	wctx, cancel := context.WithCancel(ctx)
	w := &Watcher{fsw: fsw, cancel: cancel, done: make(chan struct{})}
	go w.loop(wctx, onChange)
	return w, nil
}

// Close stops the watcher and releases its resources.
func (w *Watcher) Close() error {
	w.cancel()
	<-w.done
	return w.fsw.Close()
}

func (w *Watcher) loop(ctx context.Context, onChange func()) {
	defer close(w.done)

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
			if !isCandidateFile(ev.Name) {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(debounceWindow)
			} else {
				timer.Reset(debounceWindow)
			}
		case <-timerC():
			timer = nil
			onChange()
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			log.Printf("projectdocs watcher: %v", err)
		}
	}
}

func isCandidateFile(path string) bool {
	base := filepath.Base(path)
	for _, name := range candidateFiles {
		if base == filepath.Base(name) {
			return true
		}
	}
	return false
}
