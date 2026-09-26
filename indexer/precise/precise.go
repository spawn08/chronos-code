// Package precise is the chronos indexer's type-checked tier (M4). In the
// background, it loads Go packages with go/packages and records, per
// package directory:
//
//   - for every call site (the call's opening parenthesis, as the syntactic
//     extractor records it), the declaration go/types says it calls, and
//     the content hash of the file it was parsed from;
//   - for every named type, its method set with signatures, and the hashes
//     of the files the method sets come from.
//
// These are facts, not edges: queries use a call's target only while the
// file's indexed hash equals the recorded one, and match interfaces
// against method sets only while every contributing file is current, so a
// stale precise fact is never used and the syntactic answer applies
// instead. Nothing here runs on the edit or query path.
package precise

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/cespare/xxhash/v2"
	"golang.org/x/tools/go/packages"
)

// Version changes whenever recorded facts change shape or meaning; stores
// written by another version are discarded.
const Version = "precise-1"

// Dir is the precise facts of one package directory from one load.
type Dir struct {
	Path  string          // root-relative directory, "." for the root
	Pkg   string          // import path of the directory's package
	Files map[string]File // root-relative path -> the file's call facts
	// Types are the package's named types declared in non-test files,
	// with their method sets. API holds the hashes of the files those
	// method sets were computed from.
	Types []Type
	API   map[string]uint64
	// Defs, for facts imported from SCIP (package scip), holds the hashes
	// of the files declaring the calls' targets: a target is located by
	// its definition line only while its file is unchanged.
	Defs map[string]uint64
}

// File is the facts of one parsed file.
type File struct {
	Hash  uint64 // xxh64 of the parsed bytes
	Calls []Call // sorted by line, then column
	// TypeRefs are the identifiers naming a type (T{}, var x T, pkg.T),
	// keyed by the identifier's position, as the extractor records type
	// references; Recv is unused.
	TypeRefs []Call
}

// Call is one call site and the declaration it calls. File is "" when the
// callee has no declaration in the workspace: the standard library or a
// dependency, a builtin, or a function value.
type Call struct {
	Line, Col int32
	File      string // root-relative file declaring the callee
	Name      string
	Recv      string // the receiver's base type name, for methods (an interface's for interface methods)
	// DefLine is the 1-based line of the callee's definition, for facts
	// imported from SCIP; 0 for go/types facts.
	DefLine int32
}

// Type is a named type and its method set. For interfaces, Methods is the
// full method set (embedded interfaces included); for other types, the
// method set of *T, which includes T's and promoted methods. Each entry is
// a method key (see methodKey).
type Type struct {
	Name    string
	Iface   bool
	Methods []string // sorted
}

// Implements reports whether t's method set covers iface's.
func (t Type) Implements(iface Type) bool {
	if len(iface.Methods) == 0 || t.Iface {
		return false
	}
	for _, m := range iface.Methods {
		if _, ok := slices.BinarySearch(t.Methods, m); !ok {
			return false
		}
	}
	return true
}

// ErrNoToolchain reports that no go command is on PATH.
var ErrNoToolchain = errors.New("precise: go toolchain not found")

// Available reports whether the go command can be run.
func Available() error {
	if _, err := exec.LookPath("go"); err != nil {
		return ErrNoToolchain
	}
	return nil
}

// Result is one load's outcome.
type Result struct {
	Dirs   []*Dir            // one per directory whose packages type-checked
	Failed map[string]string // root-relative directory -> why it was left out
}

const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedForTest

// Load type-checks the packages in dirs (root-relative directories inside
// the module or workspace whose go.mod is in module, root-relative too) and
// returns their facts. dirs == nil loads every package of the module. env
// is added to the go command's environment.
func Load(ctx context.Context, root, module string, dirs []string, env []string) (*Result, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	modDir := filepath.Join(root, filepath.FromSlash(module))
	var patterns []string
	if dirs == nil {
		patterns = []string{"./..."}
	}
	for _, d := range dirs {
		rel := d
		if module != "." {
			rel = strings.TrimPrefix(strings.TrimPrefix(d, module), "/")
		}
		if rel == "" || rel == "." {
			patterns = append(patterns, ".")
		} else {
			patterns = append(patterns, "./"+rel)
		}
	}
	hashes := map[*ast.File]uint64{}
	var mu sync.Mutex
	cfg := &packages.Config{
		Context: ctx, Dir: modDir, Mode: loadMode, Tests: true,
		ParseFile: func(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
			if err == nil {
				mu.Lock()
				hashes[f] = xxhash.Sum64(src)
				mu.Unlock()
			}
			return f, err
		},
	}
	if len(env) > 0 {
		cfg.Env = append(os.Environ(), env...)
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("precise: load: %w", err)
	}
	return collect(root, select_(pkgs), hashes), nil
}

// select_ keeps one type-checked variant per package: the augmented
// in-package test variant when there is one, and external test packages.
// Test dependencies recompiled for another package and generated test
// mains are not source packages.
func select_(pkgs []*packages.Package) []*packages.Package {
	selected := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		if pkg.ForTest != "" && pkg.PkgPath != pkg.ForTest && pkg.PkgPath != pkg.ForTest+"_test" {
			continue
		}
		if pkg.Name == "main" && strings.HasSuffix(pkg.PkgPath, ".test") {
			continue
		}
		old := selected[pkg.PkgPath]
		if old == nil || (old.ForTest == "" && pkg.ForTest == pkg.PkgPath) {
			selected[pkg.PkgPath] = pkg
		}
	}
	out := make([]*packages.Package, 0, len(selected))
	for _, p := range selected {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b *packages.Package) int { return strings.Compare(a.PkgPath, b.PkgPath) })
	return out
}

func collect(root string, pkgs []*packages.Package, hashes map[*ast.File]uint64) *Result {
	res := &Result{Failed: map[string]string{}}
	byDir := map[string]*Dir{}
	rel := func(filename string) (string, bool) {
		if filename == "" || !filepath.IsAbs(filename) {
			return "", false
		}
		r, err := filepath.Rel(root, filename)
		if err != nil || r == "." || strings.HasPrefix(r, "..") {
			return "", false
		}
		return filepath.ToSlash(r), strings.HasSuffix(r, ".go")
	}
	for _, pkg := range pkgs {
		var dir string
		for _, f := range pkg.GoFiles {
			if r, ok := rel(f); ok {
				dir = path.Dir(r)
				break
			}
		}
		if dir == "" {
			continue // outside the root, or no Go files
		}
		if len(pkg.Errors) > 0 || pkg.TypesInfo == nil || pkg.Types == nil {
			why := "not type-checked"
			if len(pkg.Errors) > 0 {
				why = pkg.Errors[0].Error()
			}
			res.Failed[dir] = why
			continue
		}
		d := byDir[dir]
		if d == nil {
			d = &Dir{Path: dir, Files: map[string]File{}}
			byDir[dir] = d
		}
		test := strings.HasSuffix(pkg.PkgPath, "_test")
		if !test {
			d.Pkg = pkg.PkgPath
		}
		for _, f := range pkg.Syntax {
			tf := pkg.Fset.File(f.Pos())
			if tf == nil {
				continue
			}
			r, ok := rel(tf.Name())
			if !ok {
				continue
			}
			if _, done := d.Files[r]; done {
				continue
			}
			cs, ts := calls(pkg, f, rel)
			d.Files[r] = File{Hash: hashes[f], Calls: cs, TypeRefs: ts}
		}
		if !test {
			d.Types, d.API = methodSets(pkg, hashes, rel)
		}
	}
	for dir := range res.Failed {
		delete(byDir, dir)
	}
	for _, d := range byDir {
		res.Dirs = append(res.Dirs, d)
	}
	slices.SortFunc(res.Dirs, func(a, b *Dir) int { return strings.Compare(a.Path, b.Path) })
	return res
}

// calls records every call the syntactic extractor records (a call of an
// identifier or a selector, index expressions unwrapped), keyed by the
// position of its opening parenthesis, and every identifier naming a
// declared type, keyed by its position.
func calls(pkg *packages.Package, f *ast.File, rel func(string) (string, bool)) (out, typeRefs []Call) {
	info := pkg.TypesInfo
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if tn, ok := info.Uses[id].(*types.TypeName); ok && tn.Pkg() != nil {
				if _, param := tn.Type().(*types.TypeParam); !param {
					pos := pkg.Fset.Position(id.Pos())
					c := Call{Line: int32(pos.Line), Col: int32(pos.Column), Name: id.Name}
					if r, ok := rel(pkg.Fset.Position(tn.Pos()).Filename); ok {
						c.File, c.Name = r, tn.Name()
					}
					typeRefs = append(typeRefs, c)
				}
			}
			return true
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var id *ast.Ident
		var obj types.Object
		switch fn := unwrapIndex(call.Fun).(type) {
		case *ast.Ident:
			id, obj = fn, info.Uses[fn]
		case *ast.SelectorExpr:
			id = fn.Sel
			if sel, ok := info.Selections[fn]; ok {
				obj = sel.Obj()
			} else {
				obj = info.Uses[fn.Sel]
			}
		default:
			return true
		}
		pos := pkg.Fset.Position(call.Lparen)
		c := Call{Line: int32(pos.Line), Col: int32(pos.Column), Name: id.Name}
		switch o := obj.(type) {
		case *types.Func:
			o = o.Origin()
			if r, ok := rel(pkg.Fset.Position(o.Pos()).Filename); ok {
				c.File, c.Name = r, o.Name()
				if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil {
					c.Recv = baseName(sig.Recv().Type())
				}
			}
		case *types.TypeName:
			if r, ok := rel(pkg.Fset.Position(o.Pos()).Filename); ok {
				c.File, c.Name = r, o.Name()
			}
		}
		out = append(out, c)
		return true
	})
	byPos := func(a, b Call) int {
		if a.Line != b.Line {
			return int(a.Line - b.Line)
		}
		return int(a.Col - b.Col)
	}
	slices.SortFunc(out, byPos)
	slices.SortFunc(typeRefs, byPos)
	return out, typeRefs
}

func unwrapIndex(e ast.Expr) ast.Expr {
	switch v := e.(type) {
	case *ast.IndexExpr:
		return v.X
	case *ast.IndexListExpr:
		return v.X
	}
	return e
}

// baseName is the name of a receiver's named type (pointer and type
// arguments removed), or "" for an unnamed type.
func baseName(t types.Type) string {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	switch n := types.Unalias(t).(type) {
	case *types.Named:
		return n.Obj().Name()
	}
	return ""
}

// methodSets records the package's non-generic named types declared in
// non-test files, and the hashes of its non-test files.
func methodSets(pkg *packages.Package, hashes map[*ast.File]uint64, rel func(string) (string, bool)) ([]Type, map[string]uint64) {
	api := map[string]uint64{}
	for _, f := range pkg.Syntax {
		if tf := pkg.Fset.File(f.Pos()); tf != nil {
			if r, ok := rel(tf.Name()); ok && !strings.HasSuffix(r, "_test.go") {
				api[r] = hashes[f]
			}
		}
	}
	var out []Type
	scope := pkg.Types.Scope()
	for _, name := range scope.Names() {
		tn, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		if r, ok := rel(pkg.Fset.Position(tn.Pos()).Filename); !ok || strings.HasSuffix(r, "_test.go") {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok || named.TypeParams().Len() > 0 {
			continue
		}
		t := Type{Name: name}
		if iface, ok := named.Underlying().(*types.Interface); ok {
			t.Iface = true
			for i := 0; i < iface.NumMethods(); i++ {
				t.Methods = append(t.Methods, methodKey(iface.Method(i)))
			}
		} else {
			ms := types.NewMethodSet(types.NewPointer(named))
			for i := 0; i < ms.Len(); i++ {
				if fn, ok := ms.At(i).Obj().(*types.Func); ok {
					t.Methods = append(t.Methods, methodKey(fn))
				}
			}
		}
		slices.Sort(t.Methods)
		out = append(out, t)
	}
	return out, api
}

// methodKey identifies a method for interface satisfaction: its name
// (qualified by the package path when unexported) and its signature's
// parameter and result types, without names.
func methodKey(fn *types.Func) string {
	var b strings.Builder
	if !fn.Exported() && fn.Pkg() != nil {
		b.WriteString(fn.Pkg().Path())
		b.WriteByte('.')
	}
	b.WriteString(fn.Name())
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil {
		return b.String()
	}
	tuple := func(t *types.Tuple, variadic bool) {
		b.WriteByte('(')
		for i := 0; i < t.Len(); i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			typ := t.At(i).Type()
			if variadic && i == t.Len()-1 {
				b.WriteString("...")
				if s, ok := typ.(*types.Slice); ok {
					typ = s.Elem()
				}
			}
			b.WriteString(types.TypeString(typ, nil))
		}
		b.WriteByte(')')
	}
	tuple(sig.Params(), sig.Variadic())
	tuple(sig.Results(), false)
	return b.String()
}
