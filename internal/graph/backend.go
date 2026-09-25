package graph

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Backend is what the graph tools query. The SQLite *Store implements it
// (tests, and the tree-sitter tier for non-Go files); the chronos indexer
// implements it over one immutable snapshot (see index_backend.go).
//
// Paths: FindSymbols and friends return Symbol.File as stored (absolute for
// the SQLite Go tier, root-relative otherwise); methods taking a path accept
// either form where the backend can map it.
type Backend interface {
	FindSymbols(ctx context.Context, name, kind string) ([]Symbol, error)
	FindSymbolsFuzzy(ctx context.Context, substr string) ([]Symbol, error)
	Search(ctx context.Context, query string, topK int) ([]SearchResult, error)
	SymbolsInPackage(ctx context.Context, pkg string) ([]Symbol, error)
	FilesInPackage(ctx context.Context, pkg string) ([]FileRecord, error)
	SymbolsInFile(ctx context.Context, file string) ([]Symbol, error)
	CallersOf(ctx context.Context, name string) ([]string, error)
	CallersOfMany(ctx context.Context, names []string) (map[string][]string, error)
	// CallerSymbols returns up to limit declarations calling the short
	// callee name, ordered by file, line and name.
	CallerSymbols(ctx context.Context, name string, limit int) ([]Symbol, error)
	SymbolsByQualified(ctx context.Context, names []string) (map[string][]Symbol, error)
	CalleesOf(ctx context.Context, name string) ([]string, error)
	ImplementationsOf(ctx context.Context, interfaceName string) ([]string, error)
	ImportsOf(ctx context.Context, pkg string) ([]string, error)
	ImportersOf(ctx context.Context, pkg string) ([]string, error)
	PackageImports(ctx context.Context, pkg string) (string, error)
	Packages(ctx context.Context) ([]string, error)
	Stats(ctx context.Context) (Stats, error)
	FileHash(ctx context.Context, path string) (string, error)
	// FileProvenance returns the indexed content hash and mtime (Unix
	// seconds) of the first of paths (in sorted order) that is indexed.
	FileProvenance(ctx context.Context, paths ...string) (hash string, mtime int64, err error)
}

// IndexReport describes how current a backend's answers are and how its
// relationships were matched. Backends that implement Reporter attach it to
// tool results, so an agent can trust an empty answer instead of re-checking.
type IndexReport struct {
	Mode       string // "syntactic": parsed, not type-checked
	Generation uint64
	Files      int
	UpToDate   bool
	Pending    int    // changed paths not yet indexed
	Building   bool   // the first build is still running
	Partial    bool   // only the working set is indexed so far (progressive first build)
	Relations  string // how calls and implementations are matched, e.g. "name_matched"
	Error      string // last indexing error, if any
}

// Reporter is implemented by backends that can describe their freshness.
type Reporter interface {
	IndexReport() IndexReport
}

var _ Backend = (*Store)(nil)

// CallerSymbols implements Backend with evidenceCallersQuery.
func (s *Store) CallerSymbols(ctx context.Context, name string, limit int) ([]Symbol, error) {
	matches, err := s.scoredSymbols(ctx, evidenceCallersQuery, name)
	if err != nil {
		return nil, err
	}
	out := make([]Symbol, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.Symbol)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// FileProvenance implements Backend.
func (s *Store) FileProvenance(ctx context.Context, paths ...string) (string, int64, error) {
	if len(paths) == 0 {
		return "", 0, nil
	}
	args := make([]any, len(paths))
	for i, p := range paths {
		args[i] = p
	}
	var hash string
	var mtime int64
	query := `SELECT content_hash, mtime FROM files WHERE path IN (?` + strings.Repeat(", ?", len(paths)-1) + `) ORDER BY path LIMIT 1`
	err := s.rdb.QueryRowContext(ctx, query, args...).Scan(&hash, &mtime)
	if err != nil && err != sql.ErrNoRows {
		return "", 0, fmt.Errorf("read source provenance: %w", err)
	}
	return hash, mtime, nil
}

func (s *Store) scoredSymbols(ctx context.Context, query string, args ...any) ([]evidenceMatch, error) {
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query graph evidence: %w", err)
	}
	defer rows.Close()
	var matches []evidenceMatch
	for rows.Next() {
		var m evidenceMatch
		if err := rows.Scan(&m.ID, &m.Name, &m.Kind, &m.Package, &m.File, &m.Line, &m.EndLine, &m.Signature, &m.Doc, &m.Receiver, &m.score); err != nil {
			return nil, fmt.Errorf("scan graph evidence: %w", err)
		}
		matches = append(matches, m)
	}
	return matches, rows.Err()
}
