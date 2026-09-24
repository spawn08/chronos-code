package graph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
)

// RequestScope selects a fresh graph for the workspace root carried by each
// tool invocation. Graphs for request overrides are isolated from the startup
// store and reused only while their source and configuration fingerprint is
// unchanged.
type RequestScope struct {
	store *Store
	root  string

	mu      sync.Mutex
	closed  bool
	entries map[string]*requestGraph
}

type requestGraph struct {
	store       *Store
	fingerprint string
	refs        int
	stale       bool
	owned       bool
}

// NewRequestScope creates request-scoped graph tools backed by store for the
// configured workspace. Close must be called to release isolated graph stores.
func NewRequestScope(store *Store, root string) *RequestScope {
	return &RequestScope{store: store, root: root, entries: make(map[string]*requestGraph)}
}

// Tools returns graph navigation definitions that resolve their graph from the
// request-scoped builtins.WorkspaceRoot at invocation time.
func (s *RequestScope) Tools() []*tool.Definition {
	return s.wrap(Tools(s.store, s.root), Tools)
}

// ImpactTools returns impact definitions that resolve their graph and root at
// invocation time.
func (s *RequestScope) ImpactTools() []*tool.Definition {
	return s.wrap(ImpactTools(s.store, s.root), ImpactTools)
}

func (s *RequestScope) wrap(templates []*tool.Definition, build func(*Store, string) []*tool.Definition) []*tool.Definition {
	wrapped := make([]*tool.Definition, 0, len(templates))
	for _, template := range templates {
		definition := *template
		name := definition.Name
		definition.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			store, root, release, err := s.selectGraph(ctx)
			if err != nil {
				return graphUnavailable(root, err), nil
			}
			defer release()
			for _, selected := range build(store, root) {
				if selected.Name == name {
					return selected.Handler(ctx, args)
				}
			}
			return graphUnavailable(root, fmt.Errorf("tool %q is unavailable", name)), nil
		}
		wrapped = append(wrapped, &definition)
	}
	return wrapped
}

func (s *RequestScope) selectGraph(ctx context.Context) (*Store, string, func(), error) {
	root, err := canonicalGraphRoot(builtins.WorkspaceRoot(ctx, s.root))
	if err != nil {
		return nil, builtins.WorkspaceRoot(ctx, s.root), func() {}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, root, func() {}, fmt.Errorf("request graph scope is closed")
	}

	indexer := NewIndexer(s.store, root)
	snapshot, err := indexer.scan(ctx)
	if err != nil {
		return nil, root, func() {}, err
	}
	if entry := s.entries[root]; entry != nil && entry.fingerprint == snapshot.fingerprint {
		entry.refs++
		return entry.store, root, s.release(entry), nil
	}

	store, owned, err := s.buildGraph(ctx, root, snapshot)
	if err != nil {
		return nil, root, func() {}, err
	}
	entry := &requestGraph{store: store, fingerprint: snapshot.fingerprint, refs: 1, owned: owned}
	if previous := s.entries[root]; previous != nil {
		previous.stale = true
		if previous.refs == 0 && previous.owned {
			_ = previous.store.Close()
		}
	}
	s.entries[root] = entry
	return store, root, s.release(entry), nil
}

func (s *RequestScope) buildGraph(ctx context.Context, root string, snapshot graphSnapshot) (*Store, bool, error) {
	if s.store != nil {
		indexer := NewIndexer(s.store, root)
		fingerprint, _, records, err := indexer.indexState(ctx)
		if err == nil && fingerprint == snapshot.fingerprint && records != "" {
			return s.store, false, nil
		}
	}

	store, err := OpenStore(":memory:")
	if err != nil {
		return nil, false, err
	}
	indexer := NewIndexer(store, root)
	if _, err := indexer.IndexAll(ctx); err != nil {
		_ = store.Close()
		return nil, false, err
	}
	fingerprint, _, records, err := indexer.indexState(ctx)
	if err != nil || fingerprint != snapshot.fingerprint || records == "" {
		_ = store.Close()
		if err != nil {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("workspace changed while its graph was being built")
	}
	return store, true, nil
}

func (s *RequestScope) release(entry *requestGraph) func() {
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		entry.refs--
		if entry.refs == 0 && entry.owned && (entry.stale || s.closed) {
			_ = entry.store.Close()
		}
	}
}

// Close releases all isolated graph stores. The startup store remains owned by
// the caller that supplied it to NewRequestScope.
func (s *RequestScope) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var closeErr error
	for _, entry := range s.entries {
		entry.stale = true
		if entry.refs == 0 && entry.owned {
			if err := entry.store.Close(); err != nil && closeErr == nil {
				closeErr = err
			}
		}
	}
	return closeErr
}

func canonicalGraphRoot(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("inspect workspace root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace root is not a directory")
	}
	return filepath.Clean(canonical), nil
}

func graphUnavailable(root string, err error) map[string]any {
	return map[string]any{
		"available":      false,
		"workspace_root": root,
		"reason":         err.Error(),
	}
}
