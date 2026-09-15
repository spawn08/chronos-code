package graph

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"golang.org/x/tools/go/packages"
)

// Indexer builds and refreshes the code graph for a Go workspace rooted at
// Root by loading packages with go/packages (type-checked, Tier 1 analysis
// per PRD P1-007) and extracting symbols and edges into Store.
type Indexer struct {
	Store     *Store
	Root      string
	indexOnce sync.Once
	indexing  chan struct{}
}

// NewIndexer creates an indexer for the given store and workspace root.
func NewIndexer(store *Store, root string) *Indexer {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Indexer{Store: store, Root: root}
}

func (ix *Indexer) beginIndex(ctx context.Context) error {
	ix.indexOnce.Do(func() { ix.indexing = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case ix.indexing <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func graphExtension(path string) bool {
	if filepath.Ext(path) == ".go" {
		return true
	}
	for _, ext := range SupportedTreeSitterExtensions() {
		if strings.EqualFold(filepath.Ext(path), ext) {
			return true
		}
	}
	return false
}

func skipGraphDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules"
}

func graphConfigFile(path string) bool {
	name := filepath.Base(path)
	if filepath.Base(filepath.Dir(path)) == ".chronos" && (name == "config.yaml" || name == "config.yml") {
		return true
	}
	switch name {
	case "go.mod", "go.sum", "go.work", "go.work.sum", ".gitignore", "package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "pyproject.toml", "requirements.txt", "Cargo.toml", "Cargo.lock", "chronos.yaml", "chronos.yml", ".chronos.yaml", ".chronos.yml":
		return true
	}
	return strings.HasPrefix(name, "tsconfig") && strings.HasSuffix(name, ".json") || name == "jsconfig.json"
}

// graphPaths uses Git's own ignore rules (including negation, nested rules,
// and tracked ignored files). Non-repositories use the existing filtered walk.
func graphPaths(ctx context.Context, root string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(gitCtx, "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err == nil {
		var paths []string
		for _, rel := range strings.Split(string(out), "\x00") {
			if rel == "" {
				continue
			}
			skip := false
			parts := strings.Split(filepath.ToSlash(rel), "/")
			for _, part := range parts[:len(parts)-1] {
				if skipGraphDir(part) {
					skip = true
				}
			}
			if !skip {
				paths = append(paths, filepath.Join(root, rel))
			}
		}
		return paths, nil
	}
	if err := gitCtx.Err(); err != nil {
		return nil, err
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipGraphDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

type graphSnapshot struct {
	files       map[string]string // physical absolute source path -> content hash
	config      string
	fingerprint string
}

func (ix *Indexer) scan(ctx context.Context) (graphSnapshot, error) {
	snap := graphSnapshot{files: make(map[string]string)}
	paths, err := graphPaths(ctx, ix.Root)
	if err != nil {
		return snap, fmt.Errorf("scan graph paths: %w", err)
	}
	config := map[string]string{"version": "graph-lifecycle-v2", "root": ix.Root, "runtime": runtime.Version() + runtime.GOOS + runtime.GOARCH, "extensions": strings.Join(SupportedTreeSitterExtensions(), ",")}
	for _, key := range []string{"GOFLAGS", "GOOS", "GOARCH", "GOEXPERIMENT", "CGO_ENABLED", "GOWORK", "GOPATH", "GOROOT", "GOMODCACHE", "GOTOOLCHAIN", "GOPACKAGESDRIVER", "GOENV", "CC", "CXX", "CGO_CFLAGS", "CGO_LDFLAGS"} {
		config[key] = os.Getenv(key)
	}
	// Module/workspace files may be inherited from above Root.
	for dir := ix.Root; ; dir = filepath.Dir(dir) {
		for _, name := range []string{"go.mod", "go.sum", "go.work", "go.work.sum"} {
			paths = append(paths, filepath.Join(dir, name))
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	for _, name := range []string{".chronos/config.yaml", ".chronos/config.yml"} {
		paths = append(paths, filepath.Join(ix.Root, name))
	}
	goenv := os.Getenv("GOENV")
	if goenv == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			goenv = filepath.Join(dir, "go/env")
		}
	}
	if goenv != "" && goenv != "off" {
		paths = append(paths, goenv)
	}
	if work := os.Getenv("GOWORK"); filepath.IsAbs(work) {
		paths = append(paths, work, work+".sum")
	}
	seen := make(map[string]bool)
	buffer := make([]byte, 32*1024)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return snap, err
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		isConfig := graphConfigFile(path) || path == goenv || strings.Contains(filepath.ToSlash(path), "/.chronos/config.")
		if !graphExtension(path) && !isConfig {
			continue
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return snap, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		hash, err := scanFileHash(ctx, path, buffer)
		if err != nil {
			return snap, err
		}
		if isConfig {
			config[path] = hash
		}
		rel, relErr := filepath.Rel(ix.Root, path)
		inside := relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
		if inside && graphExtension(path) && !strings.Contains(filepath.ToSlash(path), "/.chronos/") {
			snap.files[path] = hash
		}
	}
	snap.config = DirMerkleHash(config)
	inputs := maps.Clone(snap.files)
	inputs["@config"] = snap.config
	snap.fingerprint = DirMerkleHash(inputs)
	return snap, nil
}

// Reuse the scan buffer across files instead of allocating every source body
// twice per reconciliation. The hash format remains compatible with FileHash.
func scanFileHash(ctx context.Context, path string, buffer []byte) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open scan source: %w", err)
	}
	defer f.Close()
	h := xxhash.New()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buffer)
		_, _ = h.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("hash scan source: %w", err)
		}
	}
	return fmt.Sprintf("%016x", h.Sum64()), nil
}

type indexedGraphFile struct{ pkg, hash string }

func (ix *Indexer) indexedFiles(ctx context.Context) (map[string]indexedGraphFile, error) {
	rows, err := ix.Store.db.QueryContext(ctx, `SELECT path, package, content_hash FROM files`)
	if err != nil {
		return nil, fmt.Errorf("read graph files: %w", err)
	}
	defer rows.Close()
	files := make(map[string]indexedGraphFile)
	for rows.Next() {
		var path string
		var file indexedGraphFile
		if err := rows.Scan(&path, &file.pkg, &file.hash); err != nil {
			return nil, err
		}
		files[path] = file
	}
	return files, rows.Err()
}

func graphRecordsFingerprint(files map[string]indexedGraphFile) string {
	hashes := make(map[string]string, len(files))
	for path, file := range files {
		// Partial type-error facts cannot certify a warm index.
		if file.hash == "" {
			return ""
		}
		hashes[path] = file.pkg + "\x00" + file.hash
	}
	return DirMerkleHash(hashes)
}

// Completion is separate from file hashes: excluded build-tag files and build
// inputs also participate. Clear completion before writes, certify only after
// a successful pass and a second scan of the same source/config snapshot.
func (ix *Indexer) indexState(ctx context.Context) (fingerprint, config, records string, err error) {
	_, err = ix.Store.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS graph_index_state (root TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, config TEXT NOT NULL, records TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS graph_index_inputs (root TEXT PRIMARY KEY, sources TEXT NOT NULL);
		CREATE INDEX IF NOT EXISTS idx_symbols_kind_package ON symbols(kind, package)`)
	if err != nil {
		return "", "", "", fmt.Errorf("create graph index state: %w", err)
	}
	err = ix.Store.db.QueryRowContext(ctx, `SELECT fingerprint, config, records FROM graph_index_state WHERE root = ?`, ix.Root).Scan(&fingerprint, &config, &records)
	if err == sql.ErrNoRows {
		err = nil
	}
	return
}

func (ix *Indexer) indexInputs(ctx context.Context) (map[string]string, error) {
	var data string
	err := ix.Store.db.QueryRowContext(ctx, `SELECT sources FROM graph_index_inputs WHERE root = ?`, ix.Root).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read graph index inputs: %w", err)
	}
	var inputs map[string]string
	if json.Unmarshal([]byte(data), &inputs) != nil {
		return nil, nil
	} // rebuild invalid derived cache
	return inputs, nil
}

func (ix *Indexer) writeIndexState(ctx context.Context, fingerprint, config, records string, inputs ...map[string]string) error {
	tx, err := ix.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin graph index state: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO graph_index_state (root, fingerprint, config, records) VALUES (?, ?, ?, ?)
		ON CONFLICT(root) DO UPDATE SET fingerprint = excluded.fingerprint, config = excluded.config, records = excluded.records`, ix.Root, fingerprint, config, records)
	if err != nil {
		return fmt.Errorf("write graph index state: %w", err)
	}
	if len(inputs) > 0 {
		data, err := json.Marshal(inputs[0])
		if err != nil {
			return fmt.Errorf("encode graph index inputs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO graph_index_inputs (root, sources) VALUES (?, ?) ON CONFLICT(root) DO UPDATE SET sources = excluded.sources`, ix.Root, string(data)); err != nil {
			return fmt.Errorf("write graph index inputs: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit graph index state: %w", err)
	}
	return nil
}

// Stats reports the outcome of an indexing pass. Skipped counts files whose
// content hash and implements relationships match the previous pass. Files
// counts files whose facts were actually replaced.
type IndexStats struct {
	Files    int
	Skipped  int
	Packages int
	Symbols  int
	Edges    int
	Elapsed  time.Duration
}

const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedForTest

// loadGoPackages keeps one type-checked variant per package: the augmented
// in-package test variant, plus the distinct external test package. Hash the
// parser's bytes, not a later disk read that could mark older facts current.
func loadGoPackages(ctx context.Context, dir string, patterns ...string) ([]*packages.Package, map[*ast.File]string, error) {
	return loadGoPackagesWithTests(ctx, dir, true, patterns...)
}

func loadGoPackagesWithTests(ctx context.Context, dir string, tests bool, patterns ...string) ([]*packages.Package, map[*ast.File]string, error) {
	hashes := make(map[*ast.File]string)
	var mu sync.Mutex
	pkgs, err := packages.Load(&packages.Config{
		Context: ctx, Dir: dir, Mode: loadMode, Tests: tests,
		ParseFile: func(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
			if err == nil {
				mu.Lock()
				hashes[file] = fmt.Sprintf("%016x", xxhash.Sum64(src))
				mu.Unlock()
			}
			return file, err
		},
	}, patterns...)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err != nil {
		return nil, nil, err
	}
	selected := make(map[string]*packages.Package)
	for _, pkg := range pkgs {
		// Test dependencies recompiled for another package are not additional
		// source packages. The generated test executable is not source either.
		if pkg.ForTest != "" && pkg.PkgPath != pkg.ForTest && pkg.PkgPath != pkg.ForTest+"_test" {
			continue
		}
		if pkg.Name == "main" && strings.HasSuffix(pkg.PkgPath, ".test") {
			generated := true
			for _, path := range pkg.GoFiles {
				if filepath.Ext(path) == ".go" && filepath.Base(path) != "_testmain.go" {
					generated = false
				}
			}
			if generated {
				continue
			}
		}
		old := selected[pkg.PkgPath]
		if old == nil || (old.ForTest == "" && pkg.ForTest == pkg.PkgPath) {
			selected[pkg.PkgPath] = pkg
		}
	}
	pkgs = nil
	for _, pkg := range selected {
		for _, loadErr := range pkg.Errors {
			if loadErr.Kind == packages.ParseError || (loadErr.Kind == packages.ListError && len(pkg.Syntax) == 0) {
				return nil, nil, fmt.Errorf("load package %s: %s", pkg.PkgPath, loadErr)
			}
		}
		pkgs = append(pkgs, pkg)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].PkgPath < pkgs[j].PkgPath })
	return pkgs, hashes, nil
}

// IndexAll (re)indexes every Go package under Root, using each file's
// content hash (internal/graph/merkle.go) to skip files that are unchanged
// since the last pass rather than unconditionally wiping and rebuilding the
// whole store (ROADMAP.md §7: <100ms single-file re-index, <30s full
// re-index). Files and packages no longer present on disk are pruned so a
// deleted file or removed directory doesn't leave stale rows behind
// forever; edges are pruned separately (PruneStaleEdges) since they are
// name-keyed rather than file-keyed.
func (ix *Indexer) IndexAll(ctx context.Context) (*IndexStats, error) {
	start := time.Now()
	if err := ix.beginIndex(ctx); err != nil {
		return nil, err
	}
	defer func() { <-ix.indexing }()
	snap, err := ix.scan(ctx)
	if err != nil {
		return nil, err
	}
	oldFiles, err := ix.indexedFiles(ctx)
	if err != nil {
		return nil, err
	}
	previous, config, records, err := ix.indexState(ctx)
	if err != nil {
		return nil, err
	}
	if previous == snap.fingerprint && records != "" && records == graphRecordsFingerprint(oldFiles) {
		return &IndexStats{Skipped: len(oldFiles), Elapsed: time.Since(start)}, nil
	}
	inputs, err := ix.indexInputs(ctx)
	if err != nil {
		return nil, err
	}
	full := config != snap.config || previous == "" || inputs == nil || records == "" || records != graphRecordsFingerprint(oldFiles)
	dirs, err := ix.changedGoDirs(ctx, oldFiles, inputs, snap.files, full)
	if err != nil {
		return nil, err
	}
	var oldTypes []string
	if !full && len(dirs) > 0 {
		oldTypes, err = ix.storedTypeShape(ctx, oldFiles, dirs)
		if err != nil {
			return nil, err
		}
	}
	if err := ix.writeIndexState(ctx, "", config, ""); err != nil {
		return nil, err
	}
	oldHashes := make(map[string]string, len(oldFiles))
	for path, file := range oldFiles {
		oldHashes[path] = file.hash
	}
	// Remove deleted/ignored sources even if the remaining Go package cannot
	// load. Unsupported languages from another build are retained.
	for path := range oldFiles {
		abs := path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(ix.Root, path)
		}
		if graphExtension(path) && snap.files[abs] == "" {
			if err := ix.Store.RemoveFile(ctx, path); err != nil {
				return nil, err
			}
		}
	}
	var pkgs []*packages.Package
	var hashes map[*ast.File]string
	pkgs, hashes, err = ix.loadDirs(ctx, dirs, snap.files, full)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	if !full && !sameStrings(oldTypes, loadedTypeShape(pkgs, snap.files)) {
		// Structural interfaces can relate packages without imports. A type or
		// method-set change therefore also needs the indexed type-bearing
		// packages, not unrelated function-only packages. Reload the union in
		// ONE Load call: go/types identities from separate calls cannot mix.
		expanded, err := ix.addTypeDirs(ctx, oldFiles, dirs)
		if err != nil {
			return nil, err
		}
		if expanded {
			pkgs, hashes, err = ix.loadDirs(ctx, dirs, snap.files, false)
			if err != nil {
				return nil, fmt.Errorf("load structural dependents: %w", err)
			}
		}
	}
	if full {
		// Source hashes alone cannot validate facts across changed build inputs.
		for path := range oldHashes {
			if filepath.Ext(path) == ".go" {
				delete(oldHashes, path)
			}
		}
	}
	stats := &IndexStats{}
	named := collectNamedTypes(pkgs)
	var interfaceScope map[string]bool
	if !full {
		interfaceScope, err = ix.loadedInterfaceScope(ctx, pkgs, oldFiles, dirs)
		if err != nil {
			return nil, err
		}
		// Include deleted interfaces captured before source pruning, even when
		// another declaration now reuses their short name.
		for _, key := range oldTypes {
			parts := strings.SplitN(key, "\x00", 5)
			if len(parts) == 5 && parts[3] == string(KindInterface) {
				interfaceScope[parts[2]] = true
			}
		}
	}
	seenFiles := make(map[string]bool, len(oldHashes))
	for path := range oldFiles {
		if filepath.Ext(path) == ".go" && !full && !dirs[filepath.Dir(path)] && snap.files[path] != "" {
			seenFiles[path] = true
			stats.Skipped++
		}
	}
	for _, pkg := range pkgs {
		// go/packages can load gitignored files for type information; only
		// workspace-selected physical sources become graph facts.
		selected := make([]*ast.File, 0, len(pkg.Syntax))
		for _, file := range pkg.Syntax {
			if snap.files[pkg.Fset.PositionFor(file.Pos(), false).Filename] != "" {
				selected = append(selected, file)
			}
		}
		pkg.Syntax = selected
		if len(selected) == 0 {
			continue
		}
		if err := ix.indexPackage(ctx, pkg, named, stats, oldHashes, hashes, seenFiles, interfaceScope); err != nil {
			return nil, fmt.Errorf("index package %s: %w", pkg.PkgPath, err)
		}
		stats.Packages++
	}
	for path := range oldFiles {
		if filepath.Ext(path) == ".go" && !seenFiles[path] {
			if err := ix.Store.RemoveFile(ctx, path); err != nil {
				return nil, fmt.Errorf("remove stale file %s: %w", path, err)
			}
		}
	}
	if err := ix.indexNonGoFiles(ctx, stats, snap.files, oldHashes); err != nil {
		return nil, err
	}
	current, err := ix.indexedFiles(ctx)
	if err != nil {
		return nil, err
	}
	seenPkgs := make(map[string]bool)
	for _, file := range current {
		seenPkgs[file.pkg] = true
	}
	existingPkgs, err := ix.Store.Packages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list stored packages: %w", err)
	}
	for _, name := range existingPkgs {
		if !seenPkgs[name] {
			if err := ix.Store.RemovePackage(ctx, name); err != nil {
				return nil, fmt.Errorf("remove stale package %s: %w", name, err)
			}
		}
	}
	if err := ix.Store.PruneStaleEdges(ctx); err != nil {
		return nil, err
	}

	after, err := ix.scan(ctx)
	if err != nil {
		return nil, err
	}
	complete := after.fingerprint == snap.fingerprint
	for path, file := range current {
		abs := path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(ix.Root, path)
		}
		if graphExtension(path) && snap.files[abs] != file.hash {
			complete = false
		}
	}
	if complete {
		if err := ix.writeIndexState(ctx, snap.fingerprint, snap.config, graphRecordsFingerprint(current), snap.files); err != nil {
			return nil, err
		}
	}
	stats.Elapsed = time.Since(start)
	return stats, nil
}

// indexNonGoFiles shares the ignore-aware snapshot with Go reconciliation.
func (ix *Indexer) indexNonGoFiles(ctx context.Context, stats *IndexStats, files, oldHashes map[string]string) error {
	paths := make([]string, 0, len(files))
	for path := range files {
		if filepath.Ext(path) != ".go" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(ix.Root, path)
		if err != nil {
			return err
		}
		if oldHashes[rel] == files[path] {
			stats.Skipped++
		} else {
			symbols, edges, err := IndexNonGoFile(ctx, ix.Store, ix.Root, rel)
			if err != nil {
				return fmt.Errorf("index non-Go file %s: %w", rel, err)
			}
			stats.Files++
			stats.Symbols += symbols
			stats.Edges += edges
		}
		// Earlier direct absolute-path indexing used a different store key.
		// Retire that alias only after the canonical replacement succeeded.
		if _, legacy := oldHashes[path]; legacy && path != rel {
			if err := ix.Store.RemoveFile(ctx, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// IndexFile uses the shared change scan so unchanged event targets cannot hide
// siblings/deletions. Go loading is scoped to changed packages and dependents;
// initial/config-changed or interrupted passes use a full load.
func (ix *Indexer) IndexFile(ctx context.Context, path string) (*IndexStats, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(ix.Root, path)
	}
	rel, err := filepath.Rel(ix.Root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("index path outside workspace: %s", path)
	}
	return ix.IndexAll(ctx)
}

// Plan from persisted file ownership/imports before type checking anything.
func (ix *Indexer) changedGoDirs(ctx context.Context, old map[string]indexedGraphFile, inputs, files map[string]string, full bool) (map[string]bool, error) {
	dirs := make(map[string]bool)
	for path, hash := range files {
		if filepath.Ext(path) == ".go" && (full || inputs[path] != hash) {
			dirs[filepath.Dir(path)] = true
		}
	}
	for path := range inputs {
		if filepath.Ext(path) == ".go" && files[path] == "" {
			dirs[filepath.Dir(path)] = true
		}
	}
	if full || len(dirs) == 0 {
		return dirs, nil
	}
	pkgDirs := make(map[string]string)
	for path, file := range old {
		if filepath.Ext(path) == ".go" {
			pkgDirs[file.pkg] = filepath.Dir(path)
		}
	}
	imports := make(map[string][]string)
	rows, err := ix.Store.db.QueryContext(ctx, `SELECT name, imports FROM packages`)
	if err != nil {
		return nil, fmt.Errorf("read package dependencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pkg, list string
		if err := rows.Scan(&pkg, &list); err != nil {
			return nil, err
		}
		if pkgDirs[pkg] != "" {
			imports[pkg] = strings.Split(list, ",")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for changed := true; changed; {
		changed = false
		for pkg, deps := range imports {
			if dirs[pkgDirs[pkg]] {
				continue
			}
			for _, dep := range deps {
				if dirs[pkgDirs[dep]] {
					dirs[pkgDirs[pkg]] = true
					changed = true
					break
				}
			}
		}
	}
	return dirs, nil
}

func (ix *Indexer) loadDirs(ctx context.Context, dirs map[string]bool, files map[string]string, full bool) ([]*packages.Package, map[*ast.File]string, error) {
	present := make(map[string]bool)
	withTests := false
	for path := range files {
		if filepath.Ext(path) == ".go" && dirs[filepath.Dir(path)] {
			present[filepath.Dir(path)] = true
			withTests = withTests || strings.HasSuffix(path, "_test.go")
		}
	}
	if len(present) == 0 {
		return nil, nil, nil
	}
	if full {
		return loadGoPackagesWithTests(ctx, ix.Root, withTests, "./...")
	}
	if len(present) == 1 {
		for dir := range present {
			return loadGoPackagesWithTests(ctx, dir, withTests, ".")
		}
	}
	patterns := make([]string, 0, len(present))
	for dir := range present {
		rel, err := filepath.Rel(ix.Root, dir)
		if err != nil {
			return nil, nil, err
		}
		patterns = append(patterns, "./"+filepath.ToSlash(rel))
	}
	sort.Strings(patterns)
	return loadGoPackagesWithTests(ctx, ix.Root, withTests, patterns...)
}

func typeShapeKey(sym Symbol) string {
	return strings.Join([]string{sym.File, sym.Package, sym.Name, string(sym.Kind), sym.Signature, sym.Receiver}, "\x00")
}

func loadedTypeShape(pkgs []*packages.Package, files map[string]string) []string {
	var shape []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			path := pkg.Fset.PositionFor(file.Pos(), false).Filename
			if files[path] == "" {
				continue
			}
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Recv != nil {
						shape = append(shape, typeShapeKey(funcSymbol(pkg, d, path, pkg.Fset)))
					}
				case *ast.GenDecl:
					if d.Tok == token.TYPE {
						for _, sym := range genDeclSymbols(pkg, d, path, pkg.Fset) {
							shape = append(shape, typeShapeKey(sym))
						}
					}
				}
			}
		}
	}
	sort.Strings(shape)
	return shape
}

func (ix *Indexer) storedTypeShape(ctx context.Context, files map[string]indexedGraphFile, dirs map[string]bool) ([]string, error) {
	pkgs := make(map[string]bool)
	for path, file := range files {
		if filepath.Ext(path) == ".go" && dirs[filepath.Dir(path)] {
			pkgs[file.pkg] = true
		}
	}
	var shape []string
	for pkg := range pkgs {
		rows, err := ix.Store.db.QueryContext(ctx, `SELECT file, package, name, kind, signature, receiver FROM symbols WHERE kind IN ('type','struct','interface','method') AND package = ?`, pkg)
		if err != nil {
			return nil, fmt.Errorf("read indexed type shape: %w", err)
		}
		for rows.Next() {
			var sym Symbol
			if err := rows.Scan(&sym.File, &sym.Package, &sym.Name, &sym.Kind, &sym.Signature, &sym.Receiver); err != nil {
				rows.Close()
				return nil, err
			}
			shape = append(shape, typeShapeKey(sym))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(shape)
	return shape, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (ix *Indexer) addTypeDirs(ctx context.Context, files map[string]indexedGraphFile, dirs map[string]bool) (bool, error) {
	rows, err := ix.Store.db.QueryContext(ctx, `SELECT DISTINCT package FROM symbols WHERE kind IN ('type','struct','interface','method')`)
	if err != nil {
		return false, fmt.Errorf("read structural dependent packages: %w", err)
	}
	defer rows.Close()
	typePkgs := make(map[string]bool)
	for rows.Next() {
		var pkg string
		if err := rows.Scan(&pkg); err != nil {
			return false, err
		}
		typePkgs[pkg] = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	expanded := false
	for path, file := range files {
		if filepath.Ext(path) == ".go" && typePkgs[file.pkg] && !dirs[filepath.Dir(path)] {
			dirs[filepath.Dir(path)] = true
			expanded = true
		}
	}
	return expanded, nil
}

func (ix *Indexer) loadedInterfaceScope(ctx context.Context, pkgs []*packages.Package, files map[string]indexedGraphFile, dirs map[string]bool) (map[string]bool, error) {
	scope := make(map[string]bool)
	for _, iface := range collectNamedTypes(pkgs).interfaces {
		scope[iface.Obj().Name()] = true
	}
	oldPkgs := make(map[string]bool)
	for path, file := range files {
		if filepath.Ext(path) == ".go" && dirs[filepath.Dir(path)] {
			oldPkgs[file.pkg] = true
		}
	}
	for pkg := range oldPkgs {
		rows, err := ix.Store.db.QueryContext(ctx, `SELECT name FROM symbols WHERE kind = 'interface' AND package = ?`, pkg)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return nil, err
			}
			scope[name] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return scope, nil
}

// indexPackage re-derives every loaded file, including unchanged siblings.
// Matching source hashes only skip persistence when symbols and all edges also
// match; semantic changes must not be mistaken for unchanged source content.
// seenFiles is marked for every file in pkg regardless of whether it was
// skipped, so callers can diff it against oldHashes to find files deleted
// since the last pass.
func (ix *Indexer) indexPackage(ctx context.Context, pkg *packages.Package, named namedTypes, stats *IndexStats, oldHashes map[string]string, hashes map[*ast.File]string, seenFiles map[string]bool, interfaceScope map[string]bool) error {
	if pkg.Types == nil || pkg.TypesInfo == nil {
		return nil
	}

	imports := make([]string, 0, len(pkg.Imports))
	for path := range pkg.Imports {
		imports = append(imports, path)
	}
	sort.Strings(imports)
	if err := ix.Store.UpsertPackage(ctx, pkg.PkgPath, strings.Join(imports, ",")); err != nil {
		return err
	}

	// Attach package-derived relationships to the concrete type's declaration,
	// not whichever file happened to be indexed last.
	implements := make(map[string][]Edge)
	for _, edge := range implementsEdges(named, pkg.PkgPath) {
		obj := pkg.Types.Scope().Lookup(edge.FromName)
		if obj != nil {
			path := pkg.Fset.PositionFor(obj.Pos(), false).Filename
			implements[path] = append(implements[path], edge)
		}
	}
	for _, file := range pkg.Syntax {
		path := pkg.Fset.PositionFor(file.Pos(), false).Filename
		if seenFiles[path] {
			return fmt.Errorf("source file %s appears in multiple package variants", path)
		}
		seenFiles[path] = true
		hash := hashes[file]
		if hash == "" {
			return fmt.Errorf("missing parsed content hash for %s", path)
		}
		// Type errors can erase Defs/Uses for otherwise unchanged declarations.
		// Preserve their last committed facts instead of replacing them with an
		// incomplete semantic view. Changed files remain uncached below.
		if pkg.IllTyped && oldHashes[path] == hash {
			stats.Skipped++
			continue
		}
		if oldHashes[path] == hash || interfaceScope != nil {
			previous, err := ix.Store.fileImplements(ctx, path)
			if err != nil {
				return err
			}
			if interfaceScope != nil {
				for _, edge := range previous {
					if !interfaceScope[edge.ToName] {
						// Preserve only edges whose concrete type still exists
						// in this file; removed/moved types must lose old edges.
						concrete := named.concretes[pkg.PkgPath+"."+edge.FromName]
						if concrete != nil && pkg.Fset.PositionFor(concrete.Obj().Pos(), false).Filename == path {
							implements[path] = append(implements[path], edge)
						}
					}
				}
			}
		}
		symbols, edges := collectFileFacts(pkg, file, path)
		edges = append(edges, implements[path]...)
		if oldHashes[path] == hash {
			same, err := ix.sameFileFacts(ctx, path, symbols, edges)
			if err != nil {
				return err
			}
			if same {
				stats.Skipped++
				continue
			}
		}
		// Keep best-effort navigation through type errors, but do not mark
		// potentially incomplete semantic facts current for a later pass.
		if pkg.IllTyped {
			hash = ""
		}
		var mtime int64
		if info, err := os.Stat(path); err == nil {
			mtime = info.ModTime().Unix()
		}
		if err := ix.Store.ReplaceFile(ctx, FileReplacement{Path: path, Package: pkg.PkgPath, Mtime: mtime, Hash: hash, Symbols: symbols, Edges: edges}); err != nil {
			return err
		}
		stats.Files++
		stats.Symbols += len(symbols)
		stats.Edges += len(edges)
	}
	return nil
}

func (ix *Indexer) sameFileFacts(ctx context.Context, path string, symbols []Symbol, edges []Edge) (bool, error) {
	previous, err := ix.Store.SymbolsInFile(ctx, path)
	if err != nil {
		return false, err
	}
	if len(previous) != len(symbols) {
		return false, nil
	}
	counts := make(map[Symbol]int, len(symbols))
	for _, sym := range symbols {
		sym.ID = 0
		counts[sym]++
	}
	for _, sym := range previous {
		sym.ID = 0
		if counts[sym] == 0 {
			return false, nil
		}
		counts[sym]--
	}
	rows, err := ix.Store.db.QueryContext(ctx, `SELECT kind, from_name, to_name FROM edges WHERE source_file = ?`, path)
	if err != nil {
		return false, fmt.Errorf("read file relationships: %w", err)
	}
	defer rows.Close()
	var previousEdges []Edge
	for rows.Next() {
		var edge Edge
		if err := rows.Scan(&edge.Kind, &edge.FromName, &edge.ToName); err != nil {
			return false, err
		}
		previousEdges = append(previousEdges, edge)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return sameEdges(previousEdges, edges), nil
}

func sameEdges(a, b []Edge) bool {
	set := func(edges []Edge) map[Edge]bool {
		out := make(map[Edge]bool, len(edges))
		for _, edge := range edges {
			out[edge] = true
		}
		return out
	}
	return maps.Equal(set(a), set(b))
}

func collectFileFacts(pkg *packages.Package, file *ast.File, path string) ([]Symbol, []Edge) {
	var symbols []Symbol
	var edges []Edge
	fset := pkg.Fset
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sym := funcSymbol(pkg, d, path, fset)
			symbols = append(symbols, sym)
			edges = append(edges, callEdges(pkg, d, qualifiedFuncName(sym))...)
		case *ast.GenDecl:
			symbols = append(symbols, genDeclSymbols(pkg, d, path, fset)...)
		}
	}
	return symbols, edges
}

func funcSymbol(pkg *packages.Package, d *ast.FuncDecl, path string, fset *token.FileSet) Symbol {
	kind := KindFunc
	receiver := ""
	if d.Recv != nil && len(d.Recv.List) > 0 {
		kind = KindMethod
		receiver = types.ExprString(d.Recv.List[0].Type)
	}
	obj := pkg.TypesInfo.Defs[d.Name]
	signature := "func " + d.Name.Name
	if obj != nil {
		signature = types.ObjectString(obj, types.RelativeTo(pkg.Types))
	}
	return Symbol{
		Name:      d.Name.Name,
		Kind:      kind,
		Package:   pkg.PkgPath,
		File:      path,
		Line:      fset.Position(d.Pos()).Line,
		EndLine:   fset.Position(d.End()).Line,
		Signature: signature,
		Doc:       strings.TrimSpace(d.Doc.Text()),
		Receiver:  receiver,
	}
}

func genDeclSymbols(pkg *packages.Package, d *ast.GenDecl, path string, fset *token.FileSet) []Symbol {
	var out []Symbol
	switch d.Tok {
	case token.TYPE:
		for _, spec := range d.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			kind := KindType
			switch ts.Type.(type) {
			case *ast.InterfaceType:
				kind = KindInterface
			case *ast.StructType:
				kind = KindStruct
			}
			doc := strings.TrimSpace(d.Doc.Text())
			if ts.Doc != nil {
				doc = strings.TrimSpace(ts.Doc.Text())
			}
			signature := "type " + ts.Name.Name + " " + types.ExprString(ts.Type)
			if obj := pkg.TypesInfo.Defs[ts.Name]; obj != nil {
				signature = types.ObjectString(obj, types.RelativeTo(pkg.Types))
			}
			out = append(out, Symbol{
				Name:      ts.Name.Name,
				Kind:      kind,
				Package:   pkg.PkgPath,
				File:      path,
				Line:      fset.Position(spec.Pos()).Line,
				EndLine:   fset.Position(spec.End()).Line,
				Signature: signature,
				Doc:       doc,
			})
		}
	case token.VAR, token.CONST:
		kind := KindVar
		if d.Tok == token.CONST {
			kind = KindConst
		}
		for _, spec := range d.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			doc := strings.TrimSpace(d.Doc.Text())
			if vs.Doc != nil {
				doc = strings.TrimSpace(vs.Doc.Text())
			}
			for _, name := range vs.Names {
				if name.Name == "_" {
					continue
				}
				out = append(out, Symbol{
					Name:    name.Name,
					Kind:    kind,
					Package: pkg.PkgPath,
					File:    path,
					Line:    fset.Position(name.Pos()).Line,
					EndLine: fset.Position(name.End()).Line,
					Doc:     doc,
				})
			}
		}
	}
	return out
}

func qualifiedFuncName(sym Symbol) string {
	if sym.Receiver != "" {
		return strings.TrimLeft(sym.Receiver, "*") + "." + sym.Name
	}
	return sym.Name
}

// callEdges walks a function body for call expressions and records a "call"
// edge from fromName to the best-effort resolved callee name. Resolution is
// name-based (not fully qualified) so lookups don't require disambiguating
// overlapping short names across packages — acceptable precision for a
// zero-LLM-cost navigation aid.
func callEdges(pkg *packages.Package, d *ast.FuncDecl, fromName string) []Edge {
	if d.Body == nil {
		return nil
	}
	seen := make(map[string]bool)
	var edges []Edge
	ast.Inspect(d.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calleeName(pkg, call.Fun)
		if name == "" || name == fromName || seen[name] {
			return true
		}
		seen[name] = true
		edges = append(edges, Edge{Kind: EdgeCall, FromName: fromName, ToName: name})
		return true
	})
	return edges
}

func calleeName(pkg *packages.Package, fun ast.Expr) string {
	switch e := fun.(type) {
	case *ast.Ident:
		obj := pkg.TypesInfo.Uses[e]
		if _, ok := obj.(*types.Func); ok {
			return e.Name
		}
		return ""
	case *ast.SelectorExpr:
		if sel, ok := pkg.TypesInfo.Selections[e]; ok {
			if fn, ok := sel.Obj().(*types.Func); ok {
				return fn.Name()
			}
			return ""
		}
		if obj := pkg.TypesInfo.Uses[e.Sel]; obj != nil {
			if _, ok := obj.(*types.Func); ok {
				return e.Sel.Name
			}
		}
		return ""
	}
	return ""
}

// namedTypes indexes named interface and concrete types across all loaded
// packages, keyed by package path, so implementsEdges can check every
// concrete type against every interface without re-walking the AST.
type namedTypes struct {
	interfaces map[string]*types.Named // qualified name -> type
	concretes  map[string]*types.Named
}

func collectNamedTypes(pkgs []*packages.Package) namedTypes {
	nt := namedTypes{interfaces: map[string]*types.Named{}, concretes: map[string]*types.Named{}}
	for _, pkg := range pkgs {
		if pkg.Types == nil {
			continue
		}
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			obj, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := obj.Type().(*types.Named)
			if !ok {
				continue
			}
			key := pkg.PkgPath + "." + name
			if _, isIface := named.Underlying().(*types.Interface); isIface {
				nt.interfaces[key] = named
			} else {
				nt.concretes[key] = named
			}
		}
	}
	return nt
}

// implementsEdges checks every concrete type declared in pkgPath against
// every interface across all loaded packages and records an "implements"
// edge (by short name) where satisfied. Only non-empty interfaces are
// checked, since every type trivially satisfies interface{}.
func implementsEdges(nt namedTypes, pkgPath string) []Edge {
	var edges []Edge
	for cKey, concrete := range nt.concretes {
		if concrete.Obj().Pkg().Path() != pkgPath {
			continue
		}
		concreteName := cKey[len(pkgPath)+1:]
		ptr := types.NewPointer(concrete)
		for iKey, iface := range nt.interfaces {
			ifaceType, ok := iface.Underlying().(*types.Interface)
			if !ok || ifaceType.NumMethods() == 0 {
				continue
			}
			if types.Implements(concrete, ifaceType) || types.Implements(ptr, ifaceType) {
				ifaceName := iKey[strings.LastIndex(iKey, ".")+1:]
				edges = append(edges, Edge{Kind: EdgeImplements, FromName: concreteName, ToName: ifaceName})
			}
		}
	}
	return edges
}
