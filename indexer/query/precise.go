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

// preciseFile returns the precise facts of fc's file if they are current.
func (v *View) preciseFile(fc *fileCtx) *precise.File {
	if fc.preciseDone {
		return fc.precise
	}
	fc.preciseDone = true
	ps := v.cache.precise
	if ps == nil || fc.meta.Lang != "go" || fc.meta.Hash == 0 {
		return nil
	}
	d := ps.Get(path.Dir(fc.meta.Path))
	if d == nil {
		return nil
	}
	if f, ok := d.Files[fc.meta.Path]; ok && f.Hash == fc.meta.Hash && len(f.Calls)+len(f.TypeRefs) > 0 {
		fc.precise = &f
	}
	return fc.precise
}

// preciseCall resolves a Go call from current precise facts. ok is false
// when there are none for this site; a site whose callee has no workspace
// declaration (the standard library, a builtin, a function value) resolves
// to nothing, type_checked.
func (v *View) preciseCall(fc *fileCtx, rec segment.RefRec) (targets []Symbol, ok bool) {
	f := v.preciseFile(fc)
	if f == nil {
		return nil, false
	}
	return v.preciseTarget(f.Calls, rec, false)
}

// preciseTypeRef resolves a Go type reference (T{}, var x T, extends of an
// embedded type) from current precise facts.
func (v *View) preciseTypeRef(fc *fileCtx, rec segment.RefRec) ([]Symbol, bool) {
	f := v.preciseFile(fc)
	if f == nil {
		return nil, false
	}
	return v.preciseTarget(f.TypeRefs, rec, true)
}

func (v *View) preciseTarget(sites []precise.Call, rec segment.RefRec, typeOnly bool) (targets []Symbol, ok bool) {
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
