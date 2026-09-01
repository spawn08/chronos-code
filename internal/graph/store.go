// Package graph implements chronos-code's code graph engine (PRD P1-007): a
// Go type-checked parser backed by a SQLite store, exposed to agents as
// zero-LLM-cost (T0) tools for structural code navigation.
package graph

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// SymbolKind classifies a graph symbol.
type SymbolKind string

const (
	KindFunc      SymbolKind = "func"
	KindMethod    SymbolKind = "method"
	KindType      SymbolKind = "type"
	KindInterface SymbolKind = "interface"
	KindStruct    SymbolKind = "struct"
	KindVar       SymbolKind = "var"
	KindConst     SymbolKind = "const"
)

// EdgeKind classifies a directed relationship between two symbols.
type EdgeKind string

const (
	EdgeCall       EdgeKind = "call"
	EdgeImplements EdgeKind = "implements"
)

// Symbol is a single declaration recorded in the graph.
type Symbol struct {
	ID        int64
	Name      string
	Kind      SymbolKind
	Package   string
	File      string
	Line      int
	EndLine   int
	Signature string
	Doc       string
	Receiver  string
}

// FileRecord is file-level metadata recorded in the graph.
type FileRecord struct {
	Path    string
	Package string
}

// Edge is a directed relationship between two symbols, referenced by name
// (not ID) so lookups don't require resolving the source side first.
type Edge struct {
	Kind     EdgeKind
	FromName string
	ToName   string
}

// Store is the SQLite-backed graph store. It is safe for concurrent readers;
// writes are serialized by capping the pool to a single connection, which is
// the standard workaround for modernc.org/sqlite's lack of built-in
// multi-writer locking.
type Store struct {
	db *sql.DB
}

// OpenStore opens (creating if needed) the graph database at path.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open graph store: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS files (
			path         TEXT PRIMARY KEY,
			package      TEXT NOT NULL,
			mtime        INTEGER NOT NULL,
			content_hash TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS packages (
			name    TEXT PRIMARY KEY,
			imports TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS symbols (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			name      TEXT NOT NULL,
			kind      TEXT NOT NULL,
			package   TEXT NOT NULL,
			file      TEXT NOT NULL,
			line      INTEGER NOT NULL,
			end_line  INTEGER NOT NULL,
			signature TEXT NOT NULL DEFAULT '',
			doc       TEXT NOT NULL DEFAULT '',
			receiver  TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_symbols_name ON symbols(name);
		CREATE INDEX IF NOT EXISTS idx_symbols_file ON symbols(file);
		CREATE INDEX IF NOT EXISTS idx_symbols_package ON symbols(package);
		CREATE TABLE IF NOT EXISTS edges (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			kind      TEXT NOT NULL,
			from_name TEXT NOT NULL,
			to_name   TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_edges_from ON edges(kind, from_name);
		CREATE INDEX IF NOT EXISTS idx_edges_to ON edges(kind, to_name);
		CREATE VIRTUAL TABLE IF NOT EXISTS symbols_fts USING fts5(
			name, signature, doc, package, file,
			content='symbols', content_rowid='id'
		);
		CREATE TRIGGER IF NOT EXISTS symbols_ai AFTER INSERT ON symbols BEGIN
			INSERT INTO symbols_fts(rowid, name, signature, doc, package, file)
			VALUES (new.id, new.name, new.signature, new.doc, new.package, new.file);
		END;
		CREATE TRIGGER IF NOT EXISTS symbols_ad AFTER DELETE ON symbols BEGIN
			INSERT INTO symbols_fts(symbols_fts, rowid, name, signature, doc, package, file)
			VALUES ('delete', old.id, old.name, old.signature, old.doc, old.package, old.file);
		END;
	`)
	if err != nil {
		return fmt.Errorf("migrate graph store: %w", err)
	}
	if err := s.addContentHashColumn(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`INSERT INTO symbols_fts(symbols_fts) VALUES ('rebuild')`); err != nil {
		return fmt.Errorf("rebuild symbols fts: %w", err)
	}
	return nil
}

// addContentHashColumn adds files.content_hash for graph.db files created
// before this column existed — CREATE TABLE IF NOT EXISTS above is a no-op
// against an already-existing files table, so pre-existing databases need
// this explicit ALTER. SQLite errors on a duplicate column, so check
// PRAGMA table_info first rather than attempting the ALTER unconditionally.
func (s *Store) addContentHashColumn() error {
	rows, err := s.db.Query(`PRAGMA table_info(files)`)
	if err != nil {
		return fmt.Errorf("inspect files schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan files schema: %w", err)
		}
		if name == "content_hash" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect files schema: %w", err)
	}
	if _, err := s.db.Exec(`ALTER TABLE files ADD COLUMN content_hash TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add content_hash column: %w", err)
	}
	return nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// Reset clears all indexed data (used before a full reindex).
func (s *Store) Reset(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM files; DELETE FROM packages; DELETE FROM symbols; DELETE FROM edges;`)
	if err != nil {
		return fmt.Errorf("reset graph store: %w", err)
	}
	return nil
}

// ClearFile removes all symbols and file/package metadata previously recorded
// for path, so an incremental reindex can re-insert fresh data without
// leaving stale rows behind. Edges are name-keyed and self-heal on the next
// full index; incremental updates leave them in place since a moved/renamed
// symbol still resolves by name for callers elsewhere in the repo.
func (s *Store) ClearFile(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM symbols WHERE file = ?`, path)
	if err != nil {
		return fmt.Errorf("clear file %s: %w", path, err)
	}
	return nil
}

// UpsertFile records (or refreshes) file-level metadata.
func (s *Store) UpsertFile(ctx context.Context, path, pkg string, mtime int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO files (path, package, mtime) VALUES (?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET package = excluded.package, mtime = excluded.mtime
	`, path, pkg, mtime)
	if err != nil {
		return fmt.Errorf("upsert file %s: %w", path, err)
	}
	return nil
}

// RemoveFile deletes all rows recorded for path — its file-level metadata
// and its symbols — used to prune files that were removed from disk since
// the last IndexAll pass now that IndexAll no longer wipes the store first.
func (s *Store) RemoveFile(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM symbols WHERE file = ?`, path); err != nil {
		return fmt.Errorf("remove file %s symbols: %w", path, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE path = ?`, path); err != nil {
		return fmt.Errorf("remove file %s: %w", path, err)
	}
	return nil
}

// RemovePackage deletes a package's row, used to prune packages whose
// directory no longer exists on disk since the last IndexAll pass.
func (s *Store) RemovePackage(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM packages WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("remove package %s: %w", name, err)
	}
	return nil
}

// PruneStaleEdges removes edges whose from_name or to_name no longer
// matches any indexed symbol. Previously IndexAll's full Reset provided
// this cleanup for free every pass; incremental IndexAll no longer wipes
// the edges table, so this replaces that guarantee explicitly.
func (s *Store) PruneStaleEdges(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM edges
		WHERE from_name NOT IN (SELECT name FROM symbols)
		   OR to_name NOT IN (SELECT name FROM symbols)
	`)
	if err != nil {
		return fmt.Errorf("prune stale edges: %w", err)
	}
	return nil
}

// UpsertFileHash records path's current content hash, called after
// (re)indexing it so the next pass can compare against it via
// AllFileHashes/FileHash to decide whether the file changed.
func (s *Store) UpsertFileHash(ctx context.Context, path, hash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO files (path, package, mtime, content_hash) VALUES (?, '', 0, ?)
		ON CONFLICT(path) DO UPDATE SET content_hash = excluded.content_hash
	`, path, hash)
	if err != nil {
		return fmt.Errorf("upsert file hash %s: %w", path, err)
	}
	return nil
}

// FileHash returns the stored content hash for path, or "" if path has no
// recorded hash (never indexed, or indexed before this column existed).
func (s *Store) FileHash(ctx context.Context, path string) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT content_hash FROM files WHERE path = ?`, path).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query file hash %s: %w", path, err)
	}
	return hash, nil
}

// AllFileHashes bulk-loads every stored path -> content_hash pair with a
// non-empty hash, for DiffTree-style comparison against a freshly built
// MerkleTree without one query per file.
func (s *Store) AllFileHashes(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, content_hash FROM files WHERE content_hash != ''`)
	if err != nil {
		return nil, fmt.Errorf("query file hashes: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var path, hash string
		if err := rows.Scan(&path, &hash); err != nil {
			return nil, fmt.Errorf("scan file hash: %w", err)
		}
		out[path] = hash
	}
	return out, rows.Err()
}

// UpsertPackage records a package's import list (comma-joined import paths).
func (s *Store) UpsertPackage(ctx context.Context, name, imports string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO packages (name, imports) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET imports = excluded.imports
	`, name, imports)
	if err != nil {
		return fmt.Errorf("upsert package %s: %w", name, err)
	}
	return nil
}

// InsertSymbol records a symbol declaration.
func (s *Store) InsertSymbol(ctx context.Context, sym Symbol) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO symbols (name, kind, package, file, line, end_line, signature, doc, receiver)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, sym.Name, string(sym.Kind), sym.Package, sym.File, sym.Line, sym.EndLine, sym.Signature, sym.Doc, sym.Receiver)
	if err != nil {
		return fmt.Errorf("insert symbol %s: %w", sym.Name, err)
	}
	return nil
}

// InsertEdge records a directed relationship, deduplicating identical rows.
func (s *Store) InsertEdge(ctx context.Context, e Edge) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO edges (kind, from_name, to_name)
		SELECT ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM edges WHERE kind = ? AND from_name = ? AND to_name = ?)
	`, string(e.Kind), e.FromName, e.ToName, string(e.Kind), e.FromName, e.ToName)
	if err != nil {
		return fmt.Errorf("insert edge %s->%s: %w", e.FromName, e.ToName, err)
	}
	return nil
}

// FindSymbols looks up symbols by exact name, optionally filtered by kind.
func (s *Store) FindSymbols(ctx context.Context, name, kind string) ([]Symbol, error) {
	query := `SELECT id, name, kind, package, file, line, end_line, signature, doc, receiver FROM symbols WHERE name = ?`
	args := []any{name}
	if kind != "" {
		query += ` AND kind = ?`
		args = append(args, kind)
	}
	return s.querySymbols(ctx, query, args...)
}

// FindSymbolsFuzzy looks up symbols whose name contains the given substring.
func (s *Store) FindSymbolsFuzzy(ctx context.Context, substr string) ([]Symbol, error) {
	return s.querySymbols(ctx, `SELECT id, name, kind, package, file, line, end_line, signature, doc, receiver
		FROM symbols WHERE name LIKE ? ORDER BY name LIMIT 25`, "%"+substr+"%")
}

// SearchResult is a symbol matched through the FTS index with its BM25 rank.
type SearchResult struct {
	Symbol
	Rank float64
}

// Search returns the top ranked symbols matching query. Invalid limits use the
// default rather than allowing an unbounded SQLite query.
func (s *Store) Search(ctx context.Context, query string, topK int) ([]SearchResult, error) {
	if topK < 1 {
		topK = 10
	}
	if topK > 100 {
		topK = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.name, s.kind, s.package, s.file, s.line, s.end_line,
			s.signature, s.doc, s.receiver, bm25(symbols_fts)
		FROM symbols_fts
		JOIN symbols s ON s.id = symbols_fts.rowid
		WHERE symbols_fts MATCH ?
		ORDER BY bm25(symbols_fts)
		LIMIT ?
	`, query, topK)
	if err != nil {
		return nil, fmt.Errorf("search symbols: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var result SearchResult
		var kind string
		if err := rows.Scan(&result.ID, &result.Name, &kind, &result.Package, &result.File,
			&result.Line, &result.EndLine, &result.Signature, &result.Doc, &result.Receiver, &result.Rank); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		result.Kind = SymbolKind(kind)
		out = append(out, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search symbols: %w", err)
	}
	return out, nil
}

// SymbolsInPackage returns all symbols declared in pkg.
func (s *Store) SymbolsInPackage(ctx context.Context, pkg string) ([]Symbol, error) {
	return s.querySymbols(ctx, `SELECT id, name, kind, package, file, line, end_line, signature, doc, receiver
		FROM symbols WHERE package = ? ORDER BY file, line`, pkg)
}

// FilesInPackage returns all files recorded for pkg, ordered by path.
func (s *Store) FilesInPackage(ctx context.Context, pkg string) ([]FileRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, package
		FROM files WHERE package = ? ORDER BY path
	`, pkg)
	if err != nil {
		return nil, fmt.Errorf("query files in package %s: %w", pkg, err)
	}
	defer rows.Close()

	var out []FileRecord
	for rows.Next() {
		var file FileRecord
		if err := rows.Scan(&file.Path, &file.Package); err != nil {
			return nil, fmt.Errorf("scan file in package %s: %w", pkg, err)
		}
		out = append(out, file)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query files in package %s: %w", pkg, err)
	}
	return out, nil
}

// SymbolsInFile returns all symbols declared in a file.
func (s *Store) SymbolsInFile(ctx context.Context, file string) ([]Symbol, error) {
	return s.querySymbols(ctx, `SELECT id, name, kind, package, file, line, end_line, signature, doc, receiver
		FROM symbols WHERE file = ? ORDER BY line`, file)
}

func (s *Store) querySymbols(ctx context.Context, query string, args ...any) ([]Symbol, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query symbols: %w", err)
	}
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		var sym Symbol
		var kind string
		if err := rows.Scan(&sym.ID, &sym.Name, &kind, &sym.Package, &sym.File, &sym.Line, &sym.EndLine, &sym.Signature, &sym.Doc, &sym.Receiver); err != nil {
			return nil, fmt.Errorf("scan symbol: %w", err)
		}
		sym.Kind = SymbolKind(kind)
		out = append(out, sym)
	}
	return out, rows.Err()
}

// CallersOf returns the distinct from_name values of "call" edges targeting name.
func (s *Store) CallersOf(ctx context.Context, name string) ([]string, error) {
	return s.edgeNames(ctx, `SELECT DISTINCT from_name FROM edges WHERE kind = ? AND to_name = ?`, string(EdgeCall), name)
}

// CalleesOf returns the distinct to_name values of "call" edges originating from name.
func (s *Store) CalleesOf(ctx context.Context, name string) ([]string, error) {
	return s.edgeNames(ctx, `SELECT DISTINCT to_name FROM edges WHERE kind = ? AND from_name = ?`, string(EdgeCall), name)
}

// ImplementationsOf returns the distinct concrete type names implementing interfaceName.
func (s *Store) ImplementationsOf(ctx context.Context, interfaceName string) ([]string, error) {
	return s.edgeNames(ctx, `SELECT DISTINCT from_name FROM edges WHERE kind = ? AND to_name = ?`, string(EdgeImplements), interfaceName)
}

func (s *Store) edgeNames(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query edges: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan edge: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// PackageImports returns the comma-joined import list recorded for pkg.
func (s *Store) PackageImports(ctx context.Context, pkg string) (string, error) {
	var imports string
	err := s.db.QueryRowContext(ctx, `SELECT imports FROM packages WHERE name = ?`, pkg).Scan(&imports)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query package imports: %w", err)
	}
	return imports, nil
}

// Packages returns all distinct package names recorded in the store.
func (s *Store) Packages(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM packages ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("query packages: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan package: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Stats reports the current size of the graph.
type Stats struct {
	Files    int
	Packages int
	Symbols  int
	Edges    int
}

// Stats returns row counts for each table, for reporting and diagnostics.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&st.Files); err != nil {
		return st, fmt.Errorf("count files: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM packages`).Scan(&st.Packages); err != nil {
		return st, fmt.Errorf("count packages: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbols`).Scan(&st.Symbols); err != nil {
		return st, fmt.Errorf("count symbols: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges`).Scan(&st.Edges); err != nil {
		return st, fmt.Errorf("count edges: %w", err)
	}
	return st, nil
}
