package graph

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spawn08/chronos-code/internal/indexer/query"
)

// indexBackend answers the graph tools from one chronos-indexer snapshot.
// Every answer within one tool call comes from the same generation. Files
// are root-relative; path arguments may be absolute or relative.
type indexBackend struct {
	view   *query.View
	root   string
	report IndexReport
}

var (
	_ Backend  = (*indexBackend)(nil)
	_ Reporter = (*indexBackend)(nil)
)

func (b *indexBackend) IndexReport() IndexReport { return b.report }

// rel maps a path argument to the index's root-relative, slash form.
func (b *indexBackend) rel(path string) string {
	if filepath.IsAbs(path) {
		if r, err := filepath.Rel(b.root, path); err == nil && !strings.HasPrefix(r, "..") {
			path = r
		}
	}
	return filepath.ToSlash(filepath.Clean(path))
}

func toSymbol(s query.Symbol) Symbol {
	return Symbol{
		ID: int64(s.ID), Name: s.Name, Kind: SymbolKind(s.Kind), Package: s.Package, File: s.File,
		Line: s.Line, EndLine: s.EndLine, Signature: s.Signature, Doc: s.Doc, Receiver: s.Receiver,
	}
}

func toSymbols(in []query.Symbol) []Symbol {
	out := make([]Symbol, len(in))
	for i, s := range in {
		out[i] = toSymbol(s)
	}
	return out
}

func (b *indexBackend) FindSymbols(ctx context.Context, name, kind string) ([]Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return toSymbols(b.view.Symbols(name, kind)), nil
}

func (b *indexBackend) FindSymbolsFuzzy(ctx context.Context, substr string) ([]Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return toSymbols(b.view.Fuzzy(substr)), nil
}

// Search clamps topK like the SQLite store (default 10, at most 100). Rank
// is the negated score, so lower ranks are better in both backends.
func (b *indexBackend) Search(ctx context.Context, q string, topK int) ([]SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if topK < 1 {
		topK = 10
	}
	topK = min(topK, 100)
	hits := b.view.Search(q, topK)
	out := make([]SearchResult, len(hits))
	for i, h := range hits {
		out[i] = SearchResult{Symbol: toSymbol(h.Symbol), Rank: -h.Score}
	}
	return out, nil
}

func (b *indexBackend) SymbolsInPackage(ctx context.Context, pkg string) ([]Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return toSymbols(b.view.PackageSymbols(pkg)), nil
}

func (b *indexBackend) FilesInPackage(ctx context.Context, pkg string) ([]FileRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	files := b.view.PackageFiles(pkg)
	out := make([]FileRecord, len(files))
	for i, f := range files {
		out[i] = FileRecord{Path: f.Path, Package: f.Package}
	}
	return out, nil
}

func (b *indexBackend) SymbolsInFile(ctx context.Context, file string) ([]Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return toSymbols(b.view.FileSymbols(b.rel(file))), nil
}

func (b *indexBackend) CallersOf(ctx context.Context, name string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.view.Callers([]string{name})[name], nil
}

func (b *indexBackend) CallersOfMany(ctx context.Context, names []string) (map[string][]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.view.Callers(names), nil
}

func (b *indexBackend) CallerSymbols(ctx context.Context, name string, limit int) ([]Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	syms := b.view.CallerSymbols(name)
	if limit > 0 && len(syms) > limit {
		syms = syms[:limit]
	}
	return toSymbols(syms), nil
}

func (b *indexBackend) SymbolsByQualified(ctx context.Context, names []string) (map[string][]Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := map[string][]Symbol{}
	for q, syms := range b.view.ByQualified(names) {
		if len(syms) > 0 {
			out[q] = toSymbols(syms)
		}
	}
	return out, nil
}

func (b *indexBackend) CalleesOf(ctx context.Context, name string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.view.Callees(name), nil
}

func (b *indexBackend) ImplementationsOf(ctx context.Context, interfaceName string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.view.Implementations(interfaceName), nil
}

func (b *indexBackend) ImportsOf(ctx context.Context, pkg string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.view.PackageDeps(pkg), nil
}

func (b *indexBackend) ImportersOf(ctx context.Context, pkg string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.view.PackageDependents(pkg), nil
}

func (b *indexBackend) PackageImports(ctx context.Context, pkg string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return strings.Join(b.view.PackageImports(pkg), ","), nil
}

func (b *indexBackend) Packages(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return slices.Clone(b.view.Packages()), nil
}

func (b *indexBackend) Stats(ctx context.Context) (Stats, error) {
	if err := ctx.Err(); err != nil {
		return Stats{}, err
	}
	st := b.view.Stats()
	return Stats{Files: st.Files, Packages: st.Packages, Symbols: st.Symbols, Edges: st.Calls}, nil
}

// FileHash returns the indexed content hash in the SQLite store's format
// (%016x of xxh64), or "" for an unindexed or unreadable file.
func (b *indexBackend) FileHash(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m, ok := b.view.FileMeta(b.rel(path))
	if !ok || m.Hash == 0 {
		return "", nil
	}
	return fmt.Sprintf("%016x", m.Hash), nil
}

func (b *indexBackend) FileProvenance(ctx context.Context, paths ...string) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	for _, p := range sorted {
		if m, ok := b.view.FileMeta(b.rel(p)); ok && m.Hash != 0 {
			return fmt.Sprintf("%016x", m.Hash), m.MtimeNS / 1e9, nil
		}
	}
	return "", 0, nil
}
