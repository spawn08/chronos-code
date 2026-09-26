package activation

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/spawn08/chronos-code/internal/graph"
)

// fakeGraph is a mutable in-memory graph.Backend for activation tests:
// symbols and call edges in insertion order, file hashes by path. Methods
// the tests do not reach are left to the embedded nil interface and panic.
type fakeGraph struct {
	graph.Backend
	mu      sync.Mutex
	symbols []graph.Symbol
	calls   [][2]string // from, to
	hashes  map[string]string
}

func newFakeGraph() *fakeGraph { return &fakeGraph{hashes: map[string]string{}} }

func (g *fakeGraph) InsertSymbol(_ context.Context, sym graph.Symbol) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	sym.ID = int64(len(g.symbols) + 1)
	g.symbols = append(g.symbols, sym)
	return nil
}

func (g *fakeGraph) InsertCall(_ context.Context, from, to string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, [2]string{from, to})
	return nil
}

func (g *fakeGraph) UpsertFileHash(_ context.Context, path, hash string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hashes[path] = hash
	return nil
}

func (g *fakeGraph) RemoveFile(_ context.Context, path string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.symbols = slices.DeleteFunc(g.symbols, func(s graph.Symbol) bool { return s.File == path })
	delete(g.hashes, path)
	return nil
}

func (g *fakeGraph) FindSymbols(_ context.Context, name, kind string) ([]graph.Symbol, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []graph.Symbol
	for _, s := range g.symbols {
		if s.Name == name && (kind == "" || string(s.Kind) == kind) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (g *fakeGraph) FindSymbolsFuzzy(_ context.Context, substr string) ([]graph.Symbol, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []graph.Symbol
	for _, s := range g.symbols {
		if strings.Contains(s.Name, substr) {
			out = append(out, s)
		}
	}
	slices.SortStableFunc(out, func(a, b graph.Symbol) int { return strings.Compare(a.Name, b.Name) })
	return out[:min(len(out), 25)], nil
}

func (g *fakeGraph) FileHash(_ context.Context, path string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hashes[path], nil
}

// edgeNames returns the distinct other ends of call edges, in insertion
// order: callers of name (callers=true) or its callees.
func (g *fakeGraph) edgeNames(name string, callers bool) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, c := range g.calls {
		from, to := c[0], c[1]
		if callers && to == name && !slices.Contains(out, from) {
			out = append(out, from)
		}
		if !callers && from == name && !slices.Contains(out, to) {
			out = append(out, to)
		}
	}
	return out
}

func (g *fakeGraph) CallersOf(_ context.Context, name string) ([]string, error) {
	return g.edgeNames(name, true), nil
}

func (g *fakeGraph) CalleesOf(_ context.Context, name string) ([]string, error) {
	return g.edgeNames(name, false), nil
}

func (g *fakeGraph) CallersOfMany(_ context.Context, names []string) (map[string][]string, error) {
	out := make(map[string][]string, len(names))
	for _, name := range names {
		if callers := g.edgeNames(name, true); len(callers) > 0 {
			slices.Sort(callers)
			out[name] = callers
		}
	}
	return out, nil
}

// symbolNamed returns the first symbol called name, or a placeholder with
// an ID derived from the name so callers stay distinct.
func (g *fakeGraph) symbolNamed(name string) graph.Symbol {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.symbols {
		if s.Name == name {
			return s
		}
	}
	var id int64 = 1 << 40
	for _, r := range name {
		id = id*31 + int64(r)
	}
	return graph.Symbol{ID: id, Name: name}
}

func (g *fakeGraph) CallerEdges(ctx context.Context, name string) ([]graph.CallEdge, error) {
	return g.IncomingCalls(ctx, []graph.Symbol{g.symbolNamed(name)})
}

func (g *fakeGraph) IncomingCalls(_ context.Context, targets []graph.Symbol) ([]graph.CallEdge, error) {
	var out []graph.CallEdge
	for _, t := range targets {
		for _, c := range g.edgeNames(t.Name, true) {
			out = append(out, graph.CallEdge{Caller: g.symbolNamed(c), Callee: t, Resolution: "import_resolved", Candidates: 1})
		}
	}
	return out, nil
}
