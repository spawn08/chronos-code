package query

import (
	"path"
	"slices"
	"strings"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

// maxReexportHops bounds how far re-exports are followed.
const maxReexportHops = 3

// target is a name declared in a unit, reached through imports and
// re-exports (which may rename it).
type target struct{ unit, name string }

// reexported returns where the file's imports naming name lead through
// re-exports: import {a} from './barrel' where the barrel has
// export {a} from './a', export {b as a} from './b' or export * from './a'.
func (v *View) reexported(fc *fileCtx, name string) []target {
	var out []target
	for _, imp := range fc.imports() {
		exported := ""
		for _, n := range imp.Names {
			if n.Alias == name || n.Alias == "" && n.Name == name {
				exported = n.Name
			}
		}
		if exported == "" && len(imp.Names) == 0 && imp.Kind != facts.ImportWildcard && lastSegment(imp.Path) == name {
			// use a::b::Name, import a.b.Name: the item's module re-exports it
			if _, parent := splitLast(imp.Path); parent != "" {
				out = append(out, v.followReexports(fc.meta.Lang, v.importUnits(fc, parent), name, 0)...)
			}
			continue
		}
		if exported == "" || exported == "default" {
			continue
		}
		out = append(out, v.followReexports(fc.meta.Lang, v.importUnits(fc, imp.Path), exported, 0)...)
	}
	return out
}

// followReexports returns the units and names that name is re-exported
// from, starting at the files of units: export records with a source
// (JS/TS export … from, Rust pub use) and, in Python, the imports of a
// package's __init__.py that list name. Memoized per generation.
func (v *View) followReexports(lang string, units []string, name string, hop int) []target {
	if len(units) == 0 || hop >= maxReexportHops {
		return nil
	}
	sorted := slices.Clone(units)
	slices.Sort(sorted)
	key := "rx\x00" + lang + "\x00" + name + "\x00" + strings.Join(sorted, "\x01")
	found := v.cache.memoUnits(v.sn.Generation(), key, func() []string {
		seen := map[target]bool{}
		add := func(next []string, src string) {
			for _, u := range next {
				seen[target{u, src}] = true
			}
			for _, t := range v.followReexports(lang, next, src, hop+1) {
				seen[t] = true
			}
		}
		for _, unit := range sorted {
			for _, p := range v.packagePaths(unit) {
				ref, ok := v.sn.Lookup(p)
				if !ok {
					continue
				}
				fc := v.fileCtx(int(ref.Seg), int(ref.File))
				if fc.meta.Lang != lang {
					continue
				}
				for _, e := range fc.sn.Exports(fc.file) {
					if e.Source == "" || (e.Name != name && e.Name != "*") {
						continue
					}
					src := name
					if e.Name == name && e.SourceName != "" {
						src = e.SourceName
					}
					add(v.importUnits(fc, e.Source), src)
				}
				if lang == "python" && path.Base(p) == "__init__.py" {
					for _, imp := range fc.imports() {
						for _, n := range imp.Names {
							if n.Alias == name || n.Alias == "" && n.Name == name {
								add(v.importUnits(fc, imp.Path), n.Name)
							}
						}
					}
				}
			}
		}
		out := make([]string, 0, len(seen))
		for t := range seen {
			out = append(out, t.unit+"\x00"+t.name)
		}
		slices.Sort(out)
		return out
	})
	out := make([]target, 0, len(found))
	for _, s := range found {
		u, n, _ := strings.Cut(s, "\x00")
		out = append(out, target{u, n})
	}
	return out
}

// declaredAt returns the live declarations of the given kinds at targets.
func (v *View) declaredAt(targets []target, kinds map[string]bool) []Symbol {
	var out []Symbol
	seen := map[uint64]bool{}
	for _, t := range targets {
		for _, s := range v.named(t.name, kinds) {
			if s.Package == t.unit && !seen[s.ID] {
				seen[s.ID] = true
				out = append(out, s)
			}
		}
	}
	sortSymbols(out)
	return out
}

// importedAs resolves name when an import binds it as an alias of another
// name (import {a as b}, from x import a as b, use a::b as c): the
// original name in the import's units or where they re-export it.
func (v *View) importedAs(fc *fileCtx, name string, kinds map[string]bool) []Symbol {
	for _, imp := range fc.imports() {
		for _, n := range imp.Names {
			if n.Alias != name || n.Name == name || n.Name == "default" || n.Name == "*" {
				continue
			}
			units := v.importUnits(fc, imp.Path)
			targets := make([]target, 0, len(units))
			for _, u := range units {
				targets = append(targets, target{u, n.Name})
			}
			if found := v.declaredAt(targets, kinds); len(found) > 0 {
				return found
			}
			if found := v.declaredAt(v.followReexports(fc.meta.Lang, units, n.Name, 0), kinds); len(found) > 0 {
				return found
			}
		}
	}
	return nil
}

// keys returns a set's members, sorted.
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
