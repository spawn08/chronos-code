package query

import (
	"path"
	"slices"
	"strings"

	"github.com/spawn08/chronos-code/indexer/facts"
	"github.com/spawn08/chronos-code/indexer/precise"
	"github.com/spawn08/chronos-code/indexer/segment"
)

// Type-checked answers (M4). The precise tier records go/types' target
// for each Go call site and each package's method sets (package precise).
// A call resolves type_checked only while its file's indexed hash equals
// the hash the facts were computed from; a package's method sets are used
// only while every one of its non-test files is unchanged and no file was
// added. Anything else falls back to the syntactic resolution.
//
// Other languages (M10) get the same per-site facts from SCIP indexes
// (package scip), gated the same way, with the target located by its
// definition line while the target's file is unchanged too.

// preciseFile returns the precise facts of fc's file if they are current:
// go/types facts for Go, SCIP facts for other languages.
func (v *View) preciseFile(fc *fileCtx) *precise.File {
	if fc.preciseDone {
		return fc.precise
	}
	fc.preciseDone = true
	ps := v.cache.precise
	if fc.meta.Lang != "go" {
		ps = v.cache.scip
	}
	if ps == nil || fc.meta.Hash == 0 {
		return nil
	}
	d := ps.Get(path.Dir(fc.meta.Path))
	if d == nil {
		return nil
	}
	if f, ok := d.Files[fc.meta.Path]; ok && f.Hash == fc.meta.Hash && len(f.Calls)+len(f.TypeRefs) > 0 {
		fc.precise, fc.preciseDefs = &f, d.Defs
	}
	return fc.precise
}

// preciseCall resolves a call from current precise facts. ok is false
// when there are none for this site; a site whose callee has no workspace
// declaration (the standard library, a builtin, a function value) resolves
// to nothing, type_checked. SCIP may record a call of a class (Python's
// Foo()) as a type reference.
func (v *View) preciseCall(fc *fileCtx, rec segment.RefRec) (targets []Symbol, ok bool) {
	f := v.preciseFile(fc)
	if f == nil {
		return nil, false
	}
	if targets, ok := v.preciseTarget(fc, f.Calls, rec, false); ok || fc.meta.Lang == "go" {
		return targets, ok
	}
	return v.preciseTarget(fc, f.TypeRefs, rec, true)
}

// preciseTypeRef resolves a type reference (T{}, var x T, extends of an
// embedded type) from current precise facts. SCIP records an
// instantiation (new Foo()) as a call of the constructor.
func (v *View) preciseTypeRef(fc *fileCtx, rec segment.RefRec) ([]Symbol, bool) {
	f := v.preciseFile(fc)
	if f == nil {
		return nil, false
	}
	if targets, ok := v.preciseTarget(fc, f.TypeRefs, rec, true); ok || fc.meta.Lang == "go" || rec.Kind != facts.RefInstantiate {
		return targets, ok
	}
	return v.preciseTarget(fc, f.Calls, rec, false)
}

func (v *View) preciseTarget(fc *fileCtx, sites []precise.Call, rec segment.RefRec, typeOnly bool) (targets []Symbol, ok bool) {
	i, found := slices.BinarySearchFunc(sites, [2]int32{int32(rec.Line), int32(rec.Col)}, func(c precise.Call, k [2]int32) int {
		if c.Line != k[0] {
			return int(c.Line - k[0])
		}
		return int(c.Col - k[1])
	})
	if !found {
		return nil, false
	}
	c := sites[i]
	if c.File == "" {
		return nil, c.Name == rec.Name
	}
	if c.DefLine > 0 {
		return v.scipTarget(fc, c, typeOnly)
	}
	for _, s := range v.fileDecls(c.File)[c.Name] {
		if facts.BaseType(s.Receiver) != c.Recv {
			continue
		}
		top := c.Recv == "" && s.Receiver == "" && s.Parent == ""
		switch {
		case typeOnly && top && isTypeKind(s.Kind):
		case !typeOnly && c.Recv != "" && s.Kind == facts.KindMethod:
		case !typeOnly && top && (s.Kind == facts.KindFunc || isTypeKind(s.Kind)):
		default:
			continue
		}
		targets = append(targets, s)
	}
	if len(targets) == 0 {
		return nil, false // the declaration moved or is not indexed: syntactic
	}
	return targets, true
}

// scipCallKinds are the declarations a SCIP call target may be.
var scipCallKinds = func() map[string]bool {
	m := map[string]bool{facts.KindMethod: true}
	for k := range callableKinds {
		m[k] = true
	}
	return m
}()

// scipTarget locates a SCIP call or type target. While the target's file
// is unchanged since the import, it is the innermost declaration of that
// name spanning the definition line (or, for a constructor defined at its
// keyword, the one declaration starting on that line). Otherwise only a
// declaration unique by name in that file is used, as for go/types facts.
// Anything else, or a C++ call target scipOverloadOK refuses, falls back
// to the syntactic resolution.
func (v *View) scipTarget(fc *fileCtx, c precise.Call, typeOnly bool) ([]Symbol, bool) {
	targets, ok := v.scipLocate(fc, c, typeOnly)
	if ok && !typeOnly && !v.scipOverloadOK(targets[0]) {
		return nil, false
	}
	return targets, ok
}

// scipOverloadOK reports whether a C++ SCIP call target may be used.
// scip-clang picks overloads clang's AST resolves otherwise; where the
// member has overloads with another parameter count, the syntactic
// resolver, which selects overloads by argument count, was right more
// often (tinyxml2), so those calls keep its answer. Other languages are
// not checked: their indexers were not seen to pick wrong overloads.
func (v *View) scipOverloadOK(t Symbol) bool {
	if t.Lang != "cpp" {
		return true
	}
	for _, d := range v.Symbols(t.Name, "") {
		if d.Receiver == t.Receiver && d.Parent == t.Parent && !d.Decl && (d.File != t.File || d.Line != t.Line) && d.Params != t.Params {
			return false // an overload with another parameter count
		}
	}
	return true
}

func (v *View) scipLocate(fc *fileCtx, c precise.Call, typeOnly bool) ([]Symbol, bool) {
	kinds := scipCallKinds
	if typeOnly {
		kinds = typeKinds
	}
	var named []Symbol
	for _, s := range v.fileDecls(c.File)[c.Name] {
		if kinds[s.Kind] {
			named = append(named, s)
		}
	}
	line := int(c.DefLine)
	if m, ok := v.sn.Meta(c.File); ok && m.Hash != 0 && fc.preciseDefs[c.File] == m.Hash {
		var best *Symbol
		for i, s := range named {
			if s.Line <= line && line <= max(s.Line, s.EndLine) && (best == nil || s.Line > best.Line) {
				best = &named[i]
			}
		}
		if best != nil {
			return []Symbol{*best}, true
		}
		var at []Symbol
		for _, s := range v.FileSymbols(c.File) {
			if s.Line == line && kinds[s.Kind] {
				at = append(at, s)
			}
		}
		if len(at) == 1 {
			return at, true
		}
		return nil, false
	}
	if len(named) == 1 {
		return named, true
	}
	return nil, false
}

// fileDecls returns a file's declarations by name, once per view.
func (v *View) fileDecls(file string) map[string][]Symbol {
	if m, ok := v.byFile[file]; ok {
		return m
	}
	m := map[string][]Symbol{}
	for _, s := range v.FileSymbols(file) {
		m[s.Name] = append(m[s.Name], s)
	}
	if v.byFile == nil {
		v.byFile = map[string]map[string][]Symbol{}
	}
	v.byFile[file] = m
	return m
}

type validMemo struct {
	version, gen uint64
	byPkg        map[string]*precise.Dir // import path -> current facts
}

// preciseDirs returns the precise directories whose method sets are
// current in this view's generation, by import path.
func (v *View) preciseDirs() map[string]*precise.Dir {
	c := v.cache
	c.mu.Lock()
	ps, memo := c.precise, c.valid
	c.mu.Unlock()
	if ps == nil {
		return nil
	}
	ver := ps.Version()
	if memo != nil && memo.version == ver && memo.gen == v.sn.Generation() {
		return memo.byPkg
	}
	out := map[string]*precise.Dir{}
	for _, d := range ps.All() {
		if d.Pkg != "" && len(d.Types) > 0 && v.apiCurrent(d) {
			out[d.Pkg] = d
		}
	}
	c.mu.Lock()
	c.valid = &validMemo{version: ver, gen: v.sn.Generation(), byPkg: out}
	c.mu.Unlock()
	return out
}

// apiCurrent reports whether every non-test Go file of d's package is
// indexed with the hash its method sets were computed from, and no
// non-test Go file was added.
func (v *View) apiCurrent(d *precise.Dir) bool {
	for p, h := range d.API {
		if m, ok := v.sn.Meta(p); !ok || m.Hash != h {
			return false
		}
	}
	for _, p := range v.packagePaths(d.Pkg) {
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || path.Dir(p) != d.Path {
			continue
		}
		f, ok := d.Files[p]
		if m, _ := v.sn.Meta(p); !ok || f.Hash != m.Hash {
			return false
		}
	}
	return true
}

// preciseImplementations answers Implementations for Go interface it from
// current method sets: the implementing types' names, and the packages
// whose types were checked (their syntactic candidates are not needed).
// ok is false when it has no current method set.
func (v *View) preciseImplementations(it Symbol) (found map[string]bool, checked map[string]bool, ok bool) {
	dirs := v.preciseDirs()
	d := dirs[it.Package]
	if d == nil || path.Dir(it.File) != d.Path {
		return nil, nil, false
	}
	var iface *precise.Type
	for i := range d.Types {
		if d.Types[i].Iface && d.Types[i].Name == it.Name {
			iface = &d.Types[i]
		}
	}
	if iface == nil {
		return nil, nil, false
	}
	found, checked = map[string]bool{}, map[string]bool{}
	for pkg, dd := range dirs {
		checked[pkg] = true
		for _, t := range dd.Types {
			if t.Implements(*iface) {
				found[t.Name] = true
			}
		}
	}
	return found, checked, true
}
