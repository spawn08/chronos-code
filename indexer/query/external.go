package query

import (
	"cmp"
	"path"
	"slices"
	"strings"

	"github.com/spawn08/chronos-code/indexer/contracts"
	"github.com/spawn08/chronos-code/indexer/extract/manifest"
	"github.com/spawn08/chronos-code/indexer/facts"
	"github.com/spawn08/chronos-code/indexer/segment"
)

// Cross-index references (federation, M9).
//
// Each repository keeps its own index. A reference whose import names a
// module this index does not contain is external here; another
// repository's index may declare that module (a Go module path, an npm
// package name, a Cargo crate, a Java package, a Python package). The
// importing view lists such references (ExternalRefsTo, ExternalRefsFrom)
// and the declaring view resolves them (ResolveExternal,
// ExternalIncoming). Only modules the declaring index owns by name
// resolve: exact Go import paths, npm and Cargo names from manifests,
// declared packages, Python modules at a source root. The suffix matching
// used inside one repository is not applied across repositories.
//
// Contracts join across indexes by key (ContractUses, ContractNodes); the
// federation package applies the proto-package check.

// ExternalRef is a reference made in one index to a module outside it.
// Strings are owned, so it outlives the view that produced it.
type ExternalRef struct {
	Caller Symbol // the enclosing declaration
	Line   int
	Kind   uint8  // facts.RefCall or facts.RefInstantiate
	Lang   string // the referencing file's language
	// Specs are the import specs that can bring Name into scope: one for a
	// package-qualified reference or a listed import, the wildcard imports
	// otherwise.
	Specs []string
	Name  string // the declared name (an import alias is undone)
	args  uint8  // segment.RefRec.Args
}

// ExternalRefsTo returns the calls (and, for constructors, instantiations)
// that may target any of targets — declarations of another index — made
// here through an import of a module this index does not contain. Targets
// are matched by name; the declaring view decides (ExternalIncoming).
// specs are the import specs that can name the targets (the declaring
// view's ImportSpecs): files importing one of them under an alias
// (import {a as b}) are searched for the alias too.
func (v *View) ExternalRefsTo(targets []Symbol, specs []string) []ExternalRef {
	names := map[string]bool{}
	ctorNames := map[string]bool{}
	var order []string
	for _, t := range targets {
		name := t.Name
		if t.Kind == facts.KindConstructor && t.Receiver != "" {
			name = facts.BaseType(t.Receiver)
			ctorNames[name] = true
		}
		if !names[name] {
			names[name] = true
			order = append(order, name)
		}
	}
	accept := func(kind uint8, name string) bool {
		return kind == facts.RefCall || kind == facts.RefInstantiate && ctorNames[name]
	}
	var out []ExternalRef
	for _, name := range order {
		for i := 0; i < v.sn.NumSegments(); i++ {
			seg := v.sn.Segment(i)
			lo, hi := seg.RefsTo(name)
			for r := lo; r < hi; r++ {
				if !accept(seg.RefKind(r), name) || !v.sn.Live(i, seg.RefFile(r)) {
					continue
				}
				if ref, ok := v.externalRef(i, r); ok {
					out = append(out, ref)
				}
			}
		}
	}
	for _, spec := range specs {
		for i := 0; i < v.sn.NumSegments(); i++ {
			seg := v.sn.Segment(i)
			for _, ii := range seg.Importers(spec) {
				imp := seg.Import(ii)
				if !v.sn.Live(i, imp.File) {
					continue
				}
				for _, n := range imp.Names {
					if n.Alias == "" || n.Alias == n.Name || !names[n.Name] {
						continue
					}
					for _, r := range seg.RefsInFile(imp.File) {
						if seg.Ref(r).Name != n.Alias || !accept(seg.RefKind(r), n.Name) {
							continue
						}
						if ref, ok := v.externalRef(i, r); ok {
							out = append(out, ref)
						}
					}
				}
			}
		}
	}
	return out
}

// ImportSpecs returns import specs another repository can use to name
// declaration s under an alias: the npm package name of its JS/TS
// package, the dotted module paths of a Python module and its packages.
// Other languages rarely alias imported names; they return nil.
func (v *View) ImportSpecs(s Symbol) []string {
	switch s.Lang {
	case "javascript", "typescript":
		if mf := v.project().nearest(path.Dir(s.File), manifest.KindNPM); mf != nil && mf.name != "" {
			return []string{mf.name}
		}
	case "python":
		stem := strings.TrimSuffix(stripExt(s.File), "/__init__")
		stems := []string{stem}
		for _, mfs := range v.project().byDir {
			for _, mf := range mfs {
				if mf.kind != manifest.KindPyProject {
					continue
				}
				for _, imp := range mf.imports {
					if imp.Kind == facts.ImportRoot && strings.HasPrefix(stem, imp.Path+"/") {
						stems = append(stems, strings.TrimPrefix(stem, imp.Path+"/"))
					}
				}
			}
		}
		var out []string
		for _, st := range stems {
			for d := st; d != "." && d != "" && d != "/"; d = path.Dir(d) {
				out = append(out, strings.ReplaceAll(d, "/", "."))
			}
		}
		return uniq(out)
	}
	return nil
}

// ExternalRefsFrom returns the calls made inside callers through an import
// of a module this index does not contain, in callers order and then line
// order.
func (v *View) ExternalRefsFrom(callers []Symbol) []ExternalRef {
	var out []ExternalRef
	for _, c := range callers {
		for _, call := range v.Outgoing(c) {
			if call.Kind != facts.RefCall {
				continue
			}
			if ref, ok := v.externalRef(call.seg, call.ref); ok {
				out = append(out, ref)
			}
		}
	}
	return out
}

// externalRef reports whether reference r of segment i names a module
// outside this index, and describes it. A reference this index resolves
// through an import or a binding hint is local.
func (v *View) externalRef(i, r int) (ExternalRef, bool) {
	seg := v.sn.Segment(i)
	rec := seg.Ref(r)
	if rec.Enclosing < 0 {
		return ExternalRef{}, false
	}
	fc := v.fileCtx(i, rec.File)
	lang := fc.meta.Lang
	var specs []string
	name := rec.Name
	switch rec.QualKind {
	case facts.QualPackage:
		specs = []string{rec.Qualifier}
	case facts.QualNone:
		if lang == "go" {
			return ExternalRef{}, false // unqualified Go names are the package's own
		}
		var named bool
		specs, name, named = v.importsBringing(fc, rec.Name)
		if !named {
			// Wildcard imports only count when nothing here matches.
			if targets, _ := v.resolveRef(i, r); len(targets) > 0 {
				return ExternalRef{}, false
			}
		}
	default:
		return ExternalRef{}, false
	}
	specs = slices.DeleteFunc(specs, func(spec string) bool { return spec == "" || strings.HasPrefix(spec, ".") || v.localSpec(fc, spec) })
	if len(specs) == 0 {
		return ExternalRef{}, false
	}
	if targets, label := v.resolveRef(i, r); len(targets) > 0 && (label == ImportResolved || label == TypeHinted || label == TypeChecked) {
		return ExternalRef{}, false
	}
	for k := range specs {
		specs[k] = strings.Clone(specs[k])
	}
	return ExternalRef{
		Caller: v.symbol(i, rec.Enclosing), Line: rec.Line, Kind: rec.Kind, Lang: lang,
		Specs: specs, Name: strings.Clone(name), args: rec.Args,
	}, true
}

// importsBringing returns the import specs of fc that bring name into
// scope, and the declared name behind an alias. named is true for an
// import listing the name (from x import name, import {name} from 'x',
// Java import a.b.Name, Rust use a::b::name); otherwise specs are the
// file's wildcard imports.
func (v *View) importsBringing(fc *fileCtx, name string) (specs []string, declared string, named bool) {
	var wild []string
	for _, imp := range fc.imports() {
		if imp.Kind == facts.ImportWildcard {
			wild = append(wild, imp.Path)
			continue
		}
		for _, n := range imp.Names {
			if n.Alias == name || n.Alias == "" && n.Name == name {
				return []string{imp.Path}, n.Name, true
			}
		}
		if len(imp.Names) == 0 && imp.Name == "" && lastSegment(imp.Path) == name {
			_, parent := splitLast(imp.Path)
			return []string{parent}, name, true
		}
	}
	return wild, name, false
}

// localSpec reports whether spec names a module of this index, as the
// file's own resolution maps it.
func (v *View) localSpec(fc *fileCtx, spec string) bool {
	if len(v.importUnits(fc, spec)) > 0 {
		return true
	}
	if packageScoped[fc.meta.Lang] {
		_, parent := splitLast(spec)
		return v.declaredPackage(spec) || v.declaredPackage(parent)
	}
	return false
}

// ResolveExternal resolves a reference made in another index against the
// modules this index owns. Nothing is returned when none of the
// reference's import specs names one of them. Go resolves to exported
// declarations only.
func (v *View) ResolveExternal(ref ExternalRef) ([]Symbol, string) {
	kinds := callableKinds
	rec := segment.RefRec{Kind: ref.Kind, Name: ref.Name, Args: ref.args}
	for _, spec := range ref.Specs {
		units := map[string]bool{}
		for _, u := range v.ownedUnits(ref.Lang, spec) {
			units[u] = true
		}
		pkgs := map[string]bool{}
		if packageScoped[ref.Lang] {
			_, parent := splitLast(spec)
			for _, p := range []string{spec, parent} {
				if v.declaredPackage(p) {
					pkgs[p] = true
				}
			}
		}
		if len(units) == 0 && len(pkgs) == 0 {
			continue
		}
		owner := lastSegment(spec)
		var members, free []Symbol
		collect := func(units map[string]bool) {
			v.eachNamed(ref.Name, func(i, k int) {
				s := v.symbol(i, k)
				if !(units[s.Package] || s.Lang == ref.Lang && pkgs[s.PkgName]) || s.Kind == facts.KindEmbed {
					return
				}
				if ref.Lang == "go" && !s.Exported {
					return
				}
				switch {
				case s.Receiver != "" && facts.BaseType(s.Receiver) == owner:
					members = append(members, s)
				case s.Kind != facts.KindMethod && (kinds[s.Kind] || ref.Lang == "go"):
					free = append(free, s)
				}
			})
		}
		collect(units)
		if len(members) == 0 && len(free) == 0 && len(units) > 0 && ref.Lang != "go" {
			rx := map[string]bool{}
			for _, t := range v.followReexports(ref.Lang, keys(units), ref.Name, 0) {
				if t.name == ref.Name {
					rx[t.unit] = true
				}
			}
			if len(rx) > 0 {
				collect(rx)
			}
		}
		found := members
		if len(found) == 0 {
			found = free
		}
		if len(found) > 0 {
			sortSymbols(found)
			return capped(activeFirst(definitionsFirst(byArity(ref.Lang, rec, found, 0)))), ImportResolved
		}
	}
	return nil, ""
}

// ownedUnits maps a non-relative import spec to the units of this index
// that own it by name. See the package comment above.
func (v *View) ownedUnits(lang, spec string) []string {
	switch lang {
	case "go":
		if len(v.packagePaths(spec)) > 0 {
			return []string{spec}
		}
	case "javascript", "typescript":
		return v.npmPackageUnits(spec)
	case "python":
		p := strings.ReplaceAll(spec, ".", "/")
		out := v.pythonRootUnits(p)
		out = append(out, v.fileUnits([]string{p, p + "/__init__"}, true)...)
		out = append(out, v.dirUnits(p, true)...)
		return uniq(out)
	case "rust":
		segs := strings.Split(spec, "::")
		switch segs[0] {
		case "crate", "self", "super":
			return nil
		}
		return v.rustCrateUnits(segs)
	case "php":
		return v.phpUnits(spec)
	case "dart":
		if u, ok := v.dartPackageUnits(spec); ok {
			return u
		}
	case "swift":
		return v.swiftModuleUnits(spec)
	}
	return nil
}

// ExternalIncoming returns the edges from refs (made in another index) to
// targets (declarations of this index), one per (caller, callee) pair,
// ordered by callee in targets order, then caller file and line. Every
// edge is import_resolved: the reference's import names a module of this
// index.
func (v *View) ExternalIncoming(targets []Symbol, refs []ExternalRef) []CallEdge {
	type key struct{ caller, callee uint64 }
	edges := map[key]*CallEdge{}
	order := map[uint64]int{}
	for ti, t := range targets {
		if _, ok := order[t.ID]; !ok {
			order[t.ID] = ti
		}
	}
	for _, ref := range refs {
		resolved, label := v.ResolveExternal(ref)
		if len(resolved) == 0 {
			continue
		}
		for _, t := range targets {
			if ref.Kind == facts.RefInstantiate && t.Kind != facts.KindConstructor || !targetsInclude(resolved, t) {
				continue
			}
			e := CallEdge{Caller: ref.Caller, Callee: t, Line: ref.Line, Resolution: label, Candidates: len(resolved)}
			k := key{ref.Caller.ID, t.ID}
			if old, ok := edges[k]; !ok || better(e, *old) {
				edges[k] = &e
			}
		}
	}
	out := make([]CallEdge, 0, len(edges))
	for _, e := range edges {
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b CallEdge) int {
		if c := cmp.Compare(order[a.Callee.ID], order[b.Callee.ID]); c != 0 {
			return c
		}
		return compareSites(a.Caller, b.Caller, a.Line, b.Line)
	})
	return out
}

// ContractUse is one use of a contract (facts.RefContract).
type ContractUse struct {
	Caller Symbol
	Line   int
	Kind   string   // contract kind: route, rpc, message, topic or table
	Key    string   // the key as used (a route's method may differ from the node's)
	Nodes  []Symbol // the contract nodes of this index the use resolves to
}

// ContractUses returns the uses of the contract of kind with key: for a
// route, every key that joins it (ANY on either side). Ordered by file and
// line.
func (v *View) ContractUses(kind, key string) []ContractUse {
	if !facts.ContractKinds[kind] || key == "" {
		return nil
	}
	names := []string{key}
	if kind == facts.KindRoute {
		names = routeKeys(key)
	}
	var out []ContractUse
	for _, name := range names {
		for i := 0; i < v.sn.NumSegments(); i++ {
			seg := v.sn.Segment(i)
			lo, hi := seg.RefsTo(name)
			for r := lo; r < hi; r++ {
				if seg.RefKind(r) != facts.RefContract || !v.sn.Live(i, seg.RefFile(r)) {
					continue
				}
				rec := seg.Ref(r)
				if rec.Qualifier != kind || rec.Enclosing < 0 {
					continue
				}
				nodes, _ := v.resolveRef(i, r)
				out = append(out, ContractUse{Caller: v.symbol(i, rec.Enclosing), Line: rec.Line, Kind: kind, Key: strings.Clone(rec.Name), Nodes: nodes})
			}
		}
	}
	slices.SortFunc(out, func(a, b ContractUse) int { return compareSites(a.Caller, b.Caller, a.Line, b.Line) })
	return out
}

// ContractNodes returns the contract nodes of kind a use of key joins in
// this index: the exact key, and for routes the same path under ANY (or,
// for ANY, under every method).
func (v *View) ContractNodes(kind, key string) []Symbol {
	nodes, _ := v.resolveContract(kind, key)
	return nodes
}

// ContractPackage returns the package qualifying a contract node's key
// across repositories: the proto, Thrift or GraphQL package of an rpc or
// message declared in a contract file, "" otherwise (routes, topics and
// tables are global by key; nodes declared by framework recognisers do
// not know the package).
func ContractPackage(s Symbol) string {
	if s.Kind != facts.KindRPC && s.Kind != facts.KindMessage {
		return ""
	}
	switch s.Lang {
	case contracts.LangProto, contracts.LangThrift, contracts.LangGraphQL:
		return s.PkgName
	}
	return ""
}
