package mcpdiscover

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spawn08/chronos/engine/mcp"
)

const debounceWindow = 300 * time.Millisecond

// Watcher keeps the latest discovery status and watches all config sources.
type Watcher struct {
	root     string
	paths    map[string]struct{}
	fsw      *fsnotify.Watcher
	cancel   context.CancelFunc
	done     chan struct{}
	onChange func(Snapshot)

	mu       sync.RWMutex
	snapshot Snapshot
	close    sync.Once
	closeErr error
}

// Watch starts watching project and user MCP config sources. The initial
// result is available through Snapshot; onChange runs after each debounced
// source change, including invalid updates.
func Watch(ctx context.Context, root string, onChange func(Snapshot)) (*Watcher, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve MCP project root: %w", err)
	}
	paths, err := configFilePaths(root)
	if err != nil {
		return nil, fmt.Errorf("resolve MCP config paths: %w", err)
	}
	for i, path := range paths {
		paths[i], err = filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve MCP config path %s: %w", path, err)
		}
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create MCP config watcher: %w", err)
	}
	watched := make(map[string]struct{})
	for _, path := range paths {
		if err := watchNearestParent(fsw, filepath.Dir(path), watched); err != nil {
			_ = fsw.Close()
			return nil, err
		}
	}

	wctx, cancel := context.WithCancel(ctx)
	w := &Watcher{
		root:     root,
		paths:    make(map[string]struct{}, len(paths)),
		fsw:      fsw,
		cancel:   cancel,
		done:     make(chan struct{}),
		onChange: onChange,
		snapshot: Load(root),
	}
	for _, path := range paths {
		w.paths[filepath.Clean(path)] = struct{}{}
	}
	go w.loop(wctx, watched)
	return w, nil
}

// Snapshot returns an independent copy of the latest discovery status.
func (w *Watcher) Snapshot() Snapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return cloneSnapshot(w.snapshot)
}

// Close stops the watcher and releases its resources. It is safe to call more
// than once.
func (w *Watcher) Close() error {
	w.close.Do(func() {
		w.cancel()
		<-w.done
		w.closeErr = w.fsw.Close()
	})
	return w.closeErr
}

func (w *Watcher) loop(ctx context.Context, watched map[string]struct{}) {
	defer close(w.done)
	var timer *time.Timer
	var timerC <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case event, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			name, err := filepath.Abs(event.Name)
			if err != nil {
				continue
			}
			name = filepath.Clean(name)
			relevant := false
			if event.Op&fsnotify.Create != 0 {
				for target := range w.paths {
					if pathContains(target, name) {
						relevant = true
						_ = watchNearestParent(w.fsw, filepath.Dir(target), watched)
					}
				}
			}
			if _, exact := w.paths[name]; !exact && !relevant {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(debounceWindow)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounceWindow)
			}
			timerC = timer.C
		case <-timerC:
			timerC = nil
			w.reload()
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.publishError(fmt.Errorf("MCP config watcher: %w", err))
		}
	}
}

func (w *Watcher) reload() {
	next := Load(w.root)
	w.mu.Lock()
	if next.Err != nil {
		next.Servers = cloneServers(w.snapshot.Servers)
	}
	w.snapshot = cloneSnapshot(next)
	published := cloneSnapshot(w.snapshot)
	w.mu.Unlock()
	if w.onChange != nil {
		w.onChange(published)
	}
}

func (w *Watcher) publishError(err error) {
	w.mu.Lock()
	w.snapshot.Err = err
	published := cloneSnapshot(w.snapshot)
	w.mu.Unlock()
	if w.onChange != nil {
		w.onChange(published)
	}
}

func watchNearestParent(fsw *fsnotify.Watcher, dir string, watched map[string]struct{}) error {
	dir = filepath.Clean(dir)
	for {
		if _, ok := watched[dir]; ok {
			return nil
		}
		info, err := os.Stat(dir)
		if err == nil && info.IsDir() {
			if err := fsw.Add(dir); err != nil {
				return fmt.Errorf("watch MCP config directory %s: %w", dir, err)
			}
			watched[dir] = struct{}{}
			return nil
		}
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect MCP config directory %s: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fmt.Errorf("no existing parent directory for MCP config %s", dir)
		}
		dir = parent
	}
}

func pathContains(target, candidate string) bool {
	rel, err := filepath.Rel(candidate, target)
	return err == nil && rel != "." && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	return Snapshot{Servers: cloneServers(snapshot.Servers), Err: snapshot.Err}
}

func cloneServers(servers []mcp.ServerConfig) []mcp.ServerConfig {
	if servers == nil {
		return nil
	}
	clone := make([]mcp.ServerConfig, len(servers))
	copy(clone, servers)
	for i := range clone {
		clone[i].Args = append([]string(nil), servers[i].Args...)
	}
	return clone
}
