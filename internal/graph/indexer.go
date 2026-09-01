package graph

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
)

// Indexer builds and refreshes the code graph for a Go workspace rooted at
// Root by loading packages with go/packages (type-checked, Tier 1 analysis
// per PRD P1-007) and extracting symbols and edges into Store.
type Indexer struct {
	Store *Store
	Root  string
}

// NewIndexer creates an indexer for the given store and workspace root.
func NewIndexer(store *Store, root string) *Indexer {
	return &Indexer{Store: store, Root: root}
}

// Stats reports the outcome of an indexing pass. Skipped counts files whose
// content hash matched the stored hash from the previous pass — Files only
// counts files that were actually re-parsed and re-inserted.
type IndexStats struct {
	Files    int
	Skipped  int
	Packages int
	Symbols  int
	Edges    int
	Elapsed  time.Duration
}

const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports

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
	pkgs, err := packages.Load(&packages.Config{Context: ctx, Dir: ix.Root, Mode: loadMode}, "./...")
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}

	oldHashes, err := ix.Store.AllFileHashes(ctx)
	if err != nil {
		return nil, fmt.Errorf("load stored file hashes: %w", err)
	}

	stats := &IndexStats{}
	named := collectNamedTypes(pkgs)
	seenFiles := make(map[string]bool, len(oldHashes))
	seenPkgs := make(map[string]bool, len(pkgs))
	for _, pkg := range pkgs {
		if err := ix.indexPackage(ctx, pkg, named, stats, oldHashes, seenFiles); err != nil {
			return nil, fmt.Errorf("index package %s: %w", pkg.PkgPath, err)
		}
		seenPkgs[pkg.PkgPath] = true
		stats.Packages++
	}

	for path := range oldHashes {
		if !seenFiles[path] {
			if err := ix.Store.RemoveFile(ctx, path); err != nil {
				return nil, fmt.Errorf("remove stale file %s: %w", path, err)
			}
		}
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

	if err := ix.indexNonGoFiles(ctx, stats); err != nil {
		return nil, fmt.Errorf("index non-go files: %w", err)
	}
	stats.Elapsed = time.Since(start)
	return stats, nil
}

// indexNonGoFiles walks Root for files matching the Tier-2 tree-sitter
// parsers' supported extensions (PRD P2-012) and indexes each one. In the
// default build (no "treesitter" tag), SupportedTreeSitterExtensions returns
// nil, so this is a no-op — no extra filesystem walk happens at all.
func (ix *Indexer) indexNonGoFiles(ctx context.Context, stats *IndexStats) error {
	exts := SupportedTreeSitterExtensions()
	if len(exts) == 0 {
		return nil
	}
	extSet := make(map[string]bool, len(exts))
	for _, e := range exts {
		extSet[e] = true
	}

	return filepath.WalkDir(ix.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != ix.Root && (name == ".git" || name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !extSet[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		rel, relErr := filepath.Rel(ix.Root, path)
		if relErr != nil {
			return relErr
		}
		symbols, edges, indexErr := IndexNonGoFile(ctx, ix.Store, ix.Root, rel)
		if indexErr != nil {
			// Non-fatal per file: a single unparseable file shouldn't abort
			// the whole indexing pass.
			return nil
		}
		stats.Files++
		stats.Symbols += symbols
		stats.Edges += edges
		return nil
	})
}

// IndexFile incrementally reindexes the package containing path. It first
// checks path's content hash against the stored one and returns
// immediately (without ever calling packages.Load) if unchanged — the
// short-circuit that keeps fsnotify save-without-content-change events
// (common with editors that touch mtime on every save) cheap. Otherwise it
// re-derives the package from a fresh, scoped packages.Load, skipping any
// of the package's other files whose hash is still unchanged. Call edges
// are name-keyed, so stale entries from other files self-heal on the next
// full IndexAll's PruneStaleEdges rather than needing per-edge invalidation
// here.
func (ix *Indexer) IndexFile(ctx context.Context, path string) (*IndexStats, error) {
	start := time.Now()

	if hash, err := FileHash(path); err == nil {
		if stored, storeErr := ix.Store.FileHash(ctx, path); storeErr == nil && stored != "" && stored == hash {
			return &IndexStats{Elapsed: time.Since(start)}, nil
		}
	}

	dir := filepath.Dir(path)
	pkgs, err := packages.Load(&packages.Config{Context: ctx, Dir: dir, Mode: loadMode}, ".")
	if err != nil {
		return nil, fmt.Errorf("load package for %s: %w", path, err)
	}
	oldHashes, err := ix.Store.AllFileHashes(ctx)
	if err != nil {
		return nil, fmt.Errorf("load stored file hashes: %w", err)
	}
	stats := &IndexStats{}
	named := collectNamedTypes(pkgs)
	seenFiles := make(map[string]bool)
	for _, pkg := range pkgs {
		if err := ix.indexPackage(ctx, pkg, named, stats, oldHashes, seenFiles); err != nil {
			return nil, fmt.Errorf("index package %s: %w", pkg.PkgPath, err)
		}
		stats.Packages++
	}
	stats.Elapsed = time.Since(start)
	return stats, nil
}

// indexPackage (re)indexes pkg, skipping any file whose content hash
// matches oldHashes — that file's existing symbol/edge rows are left as-is.
// seenFiles is marked for every file in pkg regardless of whether it was
// skipped, so callers can diff it against oldHashes to find files deleted
// since the last pass.
func (ix *Indexer) indexPackage(ctx context.Context, pkg *packages.Package, named namedTypes, stats *IndexStats, oldHashes map[string]string, seenFiles map[string]bool) error {
	if pkg.Types == nil || pkg.TypesInfo == nil {
		return nil
	}

	imports := make([]string, 0, len(pkg.Imports))
	for path := range pkg.Imports {
		imports = append(imports, path)
	}
	if err := ix.Store.UpsertPackage(ctx, pkg.PkgPath, strings.Join(imports, ",")); err != nil {
		return err
	}

	packageChanged := false
	for i, file := range pkg.Syntax {
		if i >= len(pkg.CompiledGoFiles) {
			break
		}
		path := pkg.CompiledGoFiles[i]
		seenFiles[path] = true

		hash, hashErr := FileHash(path)
		if hashErr == nil && oldHashes[path] == hash {
			stats.Skipped++
			continue
		}
		packageChanged = true

		info, statErr := os.Stat(path)
		mtime := time.Now().Unix()
		if statErr == nil {
			mtime = info.ModTime().Unix()
		}
		if err := ix.Store.UpsertFile(ctx, path, pkg.PkgPath, mtime); err != nil {
			return err
		}
		if hashErr == nil {
			if err := ix.Store.UpsertFileHash(ctx, path, hash); err != nil {
				return err
			}
		}
		if err := ix.Store.ClearFile(ctx, path); err != nil {
			return err
		}
		stats.Files++
		if err := ix.indexFile(ctx, pkg, file, path, stats); err != nil {
			return err
		}
	}

	// implements edges are derived once per package from the named-type
	// table, not per file; only recompute them when something in the
	// package actually changed, so an untouched package's edges aren't
	// needlessly rewritten.
	if packageChanged {
		edges := implementsEdges(named, pkg.PkgPath)
		for _, e := range edges {
			if err := ix.Store.InsertEdge(ctx, e); err != nil {
				return err
			}
			stats.Edges++
		}
	}
	return nil
}

func (ix *Indexer) indexFile(ctx context.Context, pkg *packages.Package, file *ast.File, path string, stats *IndexStats) error {
	fset := pkg.Fset
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sym := funcSymbol(pkg, d, path, fset)
			if err := ix.Store.InsertSymbol(ctx, sym); err != nil {
				return err
			}
			stats.Symbols++
			edges := callEdges(pkg, d, qualifiedFuncName(sym))
			for _, e := range edges {
				if err := ix.Store.InsertEdge(ctx, e); err != nil {
					return err
				}
				stats.Edges++
			}
		case *ast.GenDecl:
			syms := genDeclSymbols(pkg, d, path, fset)
			for _, sym := range syms {
				if err := ix.Store.InsertSymbol(ctx, sym); err != nil {
					return err
				}
				stats.Symbols++
			}
		}
	}
	return nil
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
		if !strings.HasPrefix(cKey, pkgPath+".") {
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
