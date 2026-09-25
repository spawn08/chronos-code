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
// unchanged. Freshness checks reuse a per-root scan cache, so an unchanged
// workspace costs a few stat calls rather than a `git ls-files` process and a
// re-hash of every source per call.
type RequestScope struct {
	store   *Store
	root    string
	startup *Indexer

	mu      sync.Mutex
	closed  bool
	entries map[string]*requestGraph
	slots   map[string]*requestSlot
}

type requestGraph struct {
	store       *Store
	fingerprint string
	refs        int
	stale       bool
	owned       bool

	toolsMu sync.Mutex
	tools   map[string]*tool.Definition
}

// requestSlot serializes freshness checks and builds for one root without
// blocking requests for other roots, and keeps that root's scan cache warm.
type requestSlot struct {
	mu      sync.Mutex
	indexer *Indexer
	shared  bool // indexer writes the startup store
}

// NewRequestScope creates request-scoped graph tools backed by store for the
// configured workspace. Close must be called to release isolated graph stores.
func NewRequestScope(store *Store, root string) *RequestScope {
	return &RequestScope{store: store, root: root, entries: make(map[string]*requestGraph), slots: make(map[string]*requestSlot)}
}

// NewRequestScopeForIndexer is NewRequestScope for the indexer that maintains
// the startup store (typically also driven by Watch). Sharing it serializes
// request-triggered refreshes with watcher passes and shares their scan cache.
func NewRequestScopeForIndexer(ix *Indexer) *RequestScope {
	s := NewRequestScope(ix.Store, ix.Root)
	s.startup = ix
	return s
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
			entry, root, release, err := s.selectGraph(ctx)
			if err != nil {
				return graphUnavailable(root, err), nil
			}
			defer release()
			if selected := entry.tool(name, root, build); selected != nil {
				return selected.Handler(ctx, args)
			}
			return graphUnavailable(root, fmt.Errorf("tool %q is unavailable", name)), nil
		}
		wrapped = append(wrapped, &definition)
	}
	return wrapped
}

// tool returns the entry's definition for name, building the definitions for
// this graph once instead of on every invocation.
func (g *requestGraph) tool(name, root string, build func(*Store, string) []*tool.Definition) *tool.Definition {
	g.toolsMu.Lock()
	defer g.toolsMu.Unlock()
	if definition := g.tools[name]; definition != nil {
		return definition
	}
	if g.tools == nil {
		g.tools = make(map[string]*tool.Definition)
	}
	for _, definition := range build(g.store, root) {
		if g.tools[definition.Name] == nil {
			g.tools[definition.Name] = definition
		}
	}
	return g.tools[name]
}

// slotLocked returns the per-root state (s.mu held); the startup root reuses the startup
// indexer (or one bound to the startup store) only when the configured root
// already has the canonical spelling, so facts are never recorded twice under
// two path spellings.
func (s *RequestScope) slotLocked(root string) *requestSlot {
	if slot := s.slots[root]; slot != nil {
		return slot
	}
	slot := &requestSlot{}
	switch {
	case s.startup != nil && s.startup.Store != nil && filepath.Clean(s.startup.Root) == root:
		slot.indexer, slot.shared = s.startup, true
	case s.startup == nil && s.store != nil && filepath.Clean(s.root) == root:
		slot.indexer, slot.shared = NewIndexer(s.store, root), true
	default:
		slot.indexer = NewIndexer(nil, root)
	}
	s.slots[root] = slot
	return slot
}

func (s *RequestScope) selectGraph(ctx context.Context) (*requestGraph, string, func(), error) {
	root, err := canonicalGraphRoot(builtins.WorkspaceRoot(ctx, s.root))
	if err != nil {
		return nil, builtins.WorkspaceRoot(ctx, s.root), func() {}, err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, root, func() {}, fmt.Errorf("request graph scope is closed")
	}
	slot := s.slotLocked(root)
	s.mu.Unlock()

	slot.mu.Lock()
	defer slot.mu.Unlock()
	snapshot, err := slot.indexer.scan(ctx)
	if err != nil {
		return nil, root, func() {}, err
	}
	s.mu.Lock()
	if entry := s.entries[root]; entry != nil && entry.fingerprint == snapshot.fingerprint && !s.closed {
		entry.refs++
		s.mu.Unlock()
		return entry, root, s.release(entry), nil
	}
	s.mu.Unlock()

	store, owned, err := s.buildGraph(ctx, root, slot, snapshot)
	if err != nil {
		return nil, root, func() {}, err
	}
	entry := &requestGraph{store: store, fingerprint: snapshot.fingerprint, refs: 1, owned: owned}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		if owned {
			_ = store.Close()
		}
		return nil, root, func() {}, fmt.Errorf("request graph scope is closed")
	}
	if previous := s.entries[root]; previous != nil {
		previous.stale = true
		if previous.refs == 0 && previous.owned {
			_ = previous.store.Close()
		}
	}
	s.entries[root] = entry
	return entry, root, s.release(entry), nil
}

func (s *RequestScope) buildGraph(ctx context.Context, root string, slot *requestSlot, snapshot graphSnapshot) (*Store, bool, error) {
	if slot.shared {
		ix := slot.indexer
		fingerprint, _, records, err := ix.indexState(ctx)
		if err == nil && fingerprint == snapshot.fingerprint && records != "" {
			return ix.Store, false, nil
		}
		// The startup store is behind the workspace (typically an edit the
		// watcher has not reconciled yet). Refresh it incrementally through
		// the shared indexer instead of type-checking the whole workspace
		// into a throwaway in-memory graph.
		if _, err := ix.IndexAll(ctx); err != nil {
			return nil, false, err
		}
		fingerprint, _, records, err = ix.indexState(ctx)
		if err == nil && fingerprint == snapshot.fingerprint && records != "" {
			return ix.Store, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("workspace changed while its graph was being built")
	}

	store, err := OpenStore(":memory:")
	if err != nil {
		return nil, false, err
	}
	indexer := NewIndexer(store, root)
	indexer.cache = slot.indexer.cache
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
