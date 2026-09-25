package graph

import (
	"context"
	"slices"
)

// mergedBackend answers from the chronos index (Go) and the SQLite store's
// tree-sitter tier (non-Go files, local cgo builds only) together. It exists
// until the indexer extracts other languages itself (M6); the two hold
// disjoint files, so list results are concatenated and name results united.
type mergedBackend struct {
	primary Backend
	extra   Backend
}

var (
	_ Backend  = (*mergedBackend)(nil)
	_ Reporter = (*mergedBackend)(nil)
)

func (m *mergedBackend) IndexReport() IndexReport {
	if r, ok := m.primary.(Reporter); ok {
		return r.IndexReport()
	}
	return IndexReport{}
}

func both[T any](ctx context.Context, a, b func(context.Context) (T, error)) (T, T, error) {
	x, err := a(ctx)
	if err != nil {
		var zero T
		return zero, zero, err
	}
	y, err := b(ctx)
	return x, y, err
}

func unionSorted(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

func (m *mergedBackend) FindSymbols(ctx context.Context, name, kind string) ([]Symbol, error) {
	a, b, err := both(ctx, func(c context.Context) ([]Symbol, error) { return m.primary.FindSymbols(c, name, kind) },
		func(c context.Context) ([]Symbol, error) { return m.extra.FindSymbols(c, name, kind) })
	return slices.Concat(a, b), err
}

func (m *mergedBackend) FindSymbolsFuzzy(ctx context.Context, substr string) ([]Symbol, error) {
	a, b, err := both(ctx, func(c context.Context) ([]Symbol, error) { return m.primary.FindSymbolsFuzzy(c, substr) },
		func(c context.Context) ([]Symbol, error) { return m.extra.FindSymbolsFuzzy(c, substr) })
	return slices.Concat(a, b), err
}

// Search interleaves the two ranked lists: their scores are on different
// scales, so rank position is the only comparable signal.
func (m *mergedBackend) Search(ctx context.Context, query string, topK int) ([]SearchResult, error) {
	a, b, err := both(ctx, func(c context.Context) ([]SearchResult, error) { return m.primary.Search(c, query, topK) },
		func(c context.Context) ([]SearchResult, error) { return m.extra.Search(c, query, topK) })
	if err != nil {
		return nil, err
	}
	limit := topK
	if limit < 1 {
		limit = 10
	}
	limit = min(limit, 100)
	out := make([]SearchResult, 0, min(limit, len(a)+len(b)))
	for i := 0; len(out) < limit && (i < len(a) || i < len(b)); i++ {
		if i < len(a) {
			out = append(out, a[i])
		}
		if i < len(b) && len(out) < limit {
			out = append(out, b[i])
		}
	}
	return out, nil
}

func (m *mergedBackend) SymbolsInPackage(ctx context.Context, pkg string) ([]Symbol, error) {
	a, b, err := both(ctx, func(c context.Context) ([]Symbol, error) { return m.primary.SymbolsInPackage(c, pkg) },
		func(c context.Context) ([]Symbol, error) { return m.extra.SymbolsInPackage(c, pkg) })
	return slices.Concat(a, b), err
}

func (m *mergedBackend) FilesInPackage(ctx context.Context, pkg string) ([]FileRecord, error) {
	a, b, err := both(ctx, func(c context.Context) ([]FileRecord, error) { return m.primary.FilesInPackage(c, pkg) },
		func(c context.Context) ([]FileRecord, error) { return m.extra.FilesInPackage(c, pkg) })
	return slices.Concat(a, b), err
}

func (m *mergedBackend) SymbolsInFile(ctx context.Context, file string) ([]Symbol, error) {
	a, err := m.primary.SymbolsInFile(ctx, file)
	if err != nil || len(a) > 0 {
		return a, err
	}
	return m.extra.SymbolsInFile(ctx, file)
}

func (m *mergedBackend) CallersOf(ctx context.Context, name string) ([]string, error) {
	a, b, err := both(ctx, func(c context.Context) ([]string, error) { return m.primary.CallersOf(c, name) },
		func(c context.Context) ([]string, error) { return m.extra.CallersOf(c, name) })
	if len(a)+len(b) == 0 {
		return nil, err
	}
	return unionSorted(a, b), err
}

func (m *mergedBackend) CallersOfMany(ctx context.Context, names []string) (map[string][]string, error) {
	a, b, err := both(ctx, func(c context.Context) (map[string][]string, error) { return m.primary.CallersOfMany(c, names) },
		func(c context.Context) (map[string][]string, error) { return m.extra.CallersOfMany(c, names) })
	if err != nil {
		return nil, err
	}
	for name, callers := range b {
		a[name] = unionSorted(a[name], callers)
	}
	return a, nil
}

func (m *mergedBackend) CallerSymbols(ctx context.Context, name string, limit int) ([]Symbol, error) {
	a, b, err := both(ctx, func(c context.Context) ([]Symbol, error) { return m.primary.CallerSymbols(c, name, limit) },
		func(c context.Context) ([]Symbol, error) { return m.extra.CallerSymbols(c, name, limit) })
	out := slices.Concat(a, b)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

func (m *mergedBackend) SymbolsByQualified(ctx context.Context, names []string) (map[string][]Symbol, error) {
	a, b, err := both(ctx, func(c context.Context) (map[string][]Symbol, error) { return m.primary.SymbolsByQualified(c, names) },
		func(c context.Context) (map[string][]Symbol, error) { return m.extra.SymbolsByQualified(c, names) })
	if err != nil {
		return nil, err
	}
	for q, syms := range b {
		a[q] = append(a[q], syms...)
	}
	return a, nil
}

func (m *mergedBackend) CalleesOf(ctx context.Context, name string) ([]string, error) {
	a, b, err := both(ctx, func(c context.Context) ([]string, error) { return m.primary.CalleesOf(c, name) },
		func(c context.Context) ([]string, error) { return m.extra.CalleesOf(c, name) })
	return unionSorted(a, b), err
}

func (m *mergedBackend) ImplementationsOf(ctx context.Context, iface string) ([]string, error) {
	a, b, err := both(ctx, func(c context.Context) ([]string, error) { return m.primary.ImplementationsOf(c, iface) },
		func(c context.Context) ([]string, error) { return m.extra.ImplementationsOf(c, iface) })
	return unionSorted(a, b), err
}

func (m *mergedBackend) ImportsOf(ctx context.Context, pkg string) ([]string, error) {
	a, b, err := both(ctx, func(c context.Context) ([]string, error) { return m.primary.ImportsOf(c, pkg) },
		func(c context.Context) ([]string, error) { return m.extra.ImportsOf(c, pkg) })
	return unionSorted(a, b), err
}

func (m *mergedBackend) ImportersOf(ctx context.Context, pkg string) ([]string, error) {
	a, b, err := both(ctx, func(c context.Context) ([]string, error) { return m.primary.ImportersOf(c, pkg) },
		func(c context.Context) ([]string, error) { return m.extra.ImportersOf(c, pkg) })
	return unionSorted(a, b), err
}

func (m *mergedBackend) PackageImports(ctx context.Context, pkg string) (string, error) {
	a, err := m.primary.PackageImports(ctx, pkg)
	if err != nil || a != "" {
		return a, err
	}
	return m.extra.PackageImports(ctx, pkg)
}

func (m *mergedBackend) Packages(ctx context.Context) ([]string, error) {
	a, b, err := both(ctx, m.primary.Packages, m.extra.Packages)
	return unionSorted(a, b), err
}

func (m *mergedBackend) Stats(ctx context.Context) (Stats, error) {
	a, b, err := both(ctx, m.primary.Stats, m.extra.Stats)
	return Stats{Files: a.Files + b.Files, Packages: a.Packages + b.Packages, Symbols: a.Symbols + b.Symbols, Edges: a.Edges + b.Edges}, err
}

func (m *mergedBackend) FileHash(ctx context.Context, path string) (string, error) {
	a, err := m.primary.FileHash(ctx, path)
	if err != nil || a != "" {
		return a, err
	}
	return m.extra.FileHash(ctx, path)
}

func (m *mergedBackend) FileProvenance(ctx context.Context, paths ...string) (string, int64, error) {
	h, mt, err := m.primary.FileProvenance(ctx, paths...)
	if err != nil || h != "" {
		return h, mt, err
	}
	return m.extra.FileProvenance(ctx, paths...)
}
