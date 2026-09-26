package graph

import (
	"cmp"
	"context"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/cespare/xxhash/v2"

	"github.com/spawn08/chronos-code/indexer/federation"
)

// federatedBackend answers the graph tools over the primary workspace and
// the federated repositories (M9, workspace.indexer.federation). Symbol
// lookup, search and call edges fan out across every repository; package,
// file, implementation and provenance queries, and codebase_context, stay
// on the primary workspace (the embedded backend).
//
// Primary symbols are exactly what the single-repository backend returns.
// Symbols of another repository carry Repo, a File relative to the primary
// root, and an ID mixed with the repository name.
type federatedBackend struct {
	*indexBackend // the primary workspace
	ws            *federation.Workspace
	primary       string            // the primary member's name
	roots         map[string]string // member name -> canonical root
	repos         []RepoReport      // freshness of the other members
}

var (
	_ Backend  = (*federatedBackend)(nil)
	_ Reporter = (*federatedBackend)(nil)
)

// RepoReport describes one federated repository's index.
type RepoReport struct {
	Name       string
	Root       string // relative to the primary root when possible
	Generation uint64
	Files      int
	UpToDate   bool
	Error      string // why the repository is not answering, if it is not
}

func (b *federatedBackend) IndexReport() IndexReport {
	r := b.indexBackend.IndexReport()
	r.Repos = b.repos
	return r
}

func (b *federatedBackend) toSymbol(s federation.Symbol) Symbol {
	out := toSymbol(s.Symbol)
	if s.Repo == b.primary {
		return out
	}
	out.Repo = s.Repo
	out.ID = int64(xxhash.Sum64String(s.Repo + "\x00" + strconv.FormatUint(s.ID, 10)))
	if root, ok := b.roots[s.Repo]; ok {
		if rel, err := filepath.Rel(b.root, filepath.Join(root, filepath.FromSlash(s.File))); err == nil {
			out.File = filepath.ToSlash(rel)
		}
	}
	return out
}

func (b *federatedBackend) toSymbols(in []federation.Symbol) []Symbol {
	out := make([]Symbol, len(in))
	for i, s := range in {
		out[i] = b.toSymbol(s)
	}
	return out
}

// fromSymbol maps a Symbol this backend returned back to its member's
// declaration.
func (b *federatedBackend) fromSymbol(s Symbol) (federation.Symbol, bool) {
	if s.Repo == "" {
		return federation.Symbol{Repo: b.primary, Symbol: fromSymbol(s)}, true
	}
	root, ok := b.roots[s.Repo]
	if !ok {
		return federation.Symbol{}, false
	}
	rel, err := filepath.Rel(root, filepath.Join(b.root, filepath.FromSlash(s.File)))
	if err != nil {
		return federation.Symbol{}, false
	}
	rel = filepath.ToSlash(rel)
	for _, m := range b.ws.Members() {
		if m.Name != s.Repo {
			continue
		}
		for _, d := range m.View.Symbols(s.Name, string(s.Kind)) {
			if d.File == rel && d.Line == s.Line {
				return federation.Symbol{Repo: s.Repo, Symbol: d}, true
			}
		}
	}
	return federation.Symbol{}, false
}

func (b *federatedBackend) toEdges(in []federation.Edge) []CallEdge {
	out := make([]CallEdge, len(in))
	for i, e := range in {
		out[i] = CallEdge{
			Caller: b.toSymbol(e.Caller), Callee: b.toSymbol(e.Callee), Line: e.Line,
			Resolution: e.Resolution, Candidates: e.Candidates, Via: e.Via,
		}
	}
	return out
}

func (b *federatedBackend) FindSymbols(ctx context.Context, name, kind string) ([]Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.toSymbols(b.ws.Symbols(name, kind)), nil
}

func (b *federatedBackend) FindSymbolsFuzzy(ctx context.Context, substr string) ([]Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.toSymbols(b.ws.Fuzzy(substr)), nil
}

// Search clamps topK like the single-repository backend and merges the
// repositories' hits by score.
func (b *federatedBackend) Search(ctx context.Context, q string, topK int) ([]SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if topK < 1 {
		topK = 10
	}
	topK = min(topK, 100)
	hits := b.ws.Search(q, topK)
	out := make([]SearchResult, len(hits))
	for i, h := range hits {
		out[i] = SearchResult{Symbol: b.toSymbol(h.Symbol), Rank: -h.Score}
	}
	return out, nil
}

func (b *federatedBackend) CallerEdges(ctx context.Context, name string) ([]CallEdge, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.toEdges(b.ws.CallerEdges(name)), nil
}

func (b *federatedBackend) IncomingCalls(ctx context.Context, targets []Symbol) ([]CallEdge, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	in := make([]federation.Symbol, 0, len(targets))
	for _, t := range targets {
		if s, ok := b.fromSymbol(t); ok {
			in = append(in, s)
		}
	}
	return b.toEdges(b.ws.IncomingEdges(in)), nil
}

func (b *federatedBackend) CallersOf(ctx context.Context, name string) ([]string, error) {
	m, err := b.CallersOfMany(ctx, []string{name})
	return m[name], err
}

// CallersOfMany returns qualified caller identities; callers in another
// repository are prefixed "repo:".
func (b *federatedBackend) CallersOfMany(ctx context.Context, names []string) (map[string][]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(names))
	for _, name := range names {
		seen := map[string]bool{}
		for _, e := range b.ws.CallerEdges(name) {
			q := repoQualified(b.toSymbol(e.Caller))
			if q == name || seen[q] {
				continue
			}
			seen[q] = true
			out[name] = append(out[name], q)
		}
		slices.Sort(out[name])
	}
	return out, nil
}

func (b *federatedBackend) CallerSymbols(ctx context.Context, name string, limit int) ([]Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []Symbol
	for _, e := range b.ws.CallerEdges(name) {
		s := b.toSymbol(e.Caller)
		if e.Caller.Qualified() == name || seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		out = append(out, s)
	}
	slices.SortFunc(out, compareSymbols)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CalleesOf returns qualified callee identities; callees in another
// repository are prefixed "repo:".
func (b *federatedBackend) CalleesOf(ctx context.Context, name string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, s := range b.ws.Callees(name) {
		seen[repoQualified(b.toSymbol(s))] = true
	}
	out := make([]string, 0, len(seen))
	for q := range seen {
		out = append(out, q)
	}
	slices.Sort(out)
	return out, nil
}

// compareSymbols orders by file, line and name (query's site order).
func compareSymbols(a, b Symbol) int {
	if c := cmp.Compare(a.File, b.File); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Line, b.Line); c != 0 {
		return c
	}
	return cmp.Compare(a.Name, b.Name)
}
