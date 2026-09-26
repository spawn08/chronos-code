package query

import (
	"path"
	"strings"

	"github.com/spawn08/chronos-code/indexer/extract/manifest"
	"github.com/spawn08/chronos-code/indexer/facts"
)

// Language module resolution from the project manifests (M7). Each
// function maps an import spec to the workspace units (directories) it
// names, or nil when the manifests do not know it.

// maxExtends bounds tsconfig "extends" chains.
const maxExtends = 4

// jsPackageUnits resolves a bare JS/TS spec: tsconfig/jsconfig paths and
// baseUrl of the nearest config (following extends), then a workspace
// package of that name (package.json exports, main and types, mapped from
// build output back to sources).
func (v *View) jsPackageUnits(dir, spec string) []string {
	p := v.project()
	if cfg := p.nearest(dir, manifest.KindTSConfig); cfg != nil {
		var aliases []facts.Import
		base := ""
		for k, mf := 0, cfg; mf != nil && k < maxExtends; k++ {
			var own []facts.Import
			var next *manifestFile
			for _, imp := range mf.imports {
				switch imp.Kind {
				case facts.ImportAlias:
					own = append(own, imp)
				case facts.ImportRoot:
					if base == "" {
						base = imp.Path
					}
				case facts.ImportModule:
					next = p.file(imp.Path)
				}
			}
			if aliases == nil && len(own) > 0 { // a config's paths replace its base's
				aliases = own
			}
			mf = next
		}
		for _, a := range aliases {
			if target, ok := matchAlias(a.Name, a.Path, spec); ok {
				if u := v.jsFileUnits(target); len(u) > 0 {
					return u
				}
			}
		}
		if base != "" {
			if u := v.jsFileUnits(path.Join(base, spec)); len(u) > 0 {
				return u
			}
		}
	}
	return v.npmPackageUnits(spec)
}

// npmPackageUnits resolves a bare JS/TS spec to the workspace package of
// that name (package.json exports, main and types, mapped from build
// output back to sources), or nil.
func (v *View) npmPackageUnits(spec string) []string {
	p := v.project()
	name, sub := jsPackageName(spec)
	pkgDir, ok := p.named(manifest.KindNPM, name)
	if !ok {
		return nil
	}
	var targets []string
	key := "."
	if sub != "" {
		key = "./" + sub
	}
	for _, mf := range p.byDir[pkgDir] {
		if mf.kind != manifest.KindNPM {
			continue
		}
		for _, e := range mf.exports {
			if t, ok := matchAlias(e.Name, e.Source, key); ok {
				targets = append(targets, t)
			}
		}
	}
	if sub != "" {
		targets = append(targets, path.Join(pkgDir, "src", sub), path.Join(pkgDir, sub))
	} else {
		targets = append(targets, path.Join(pkgDir, "src", "index"), path.Join(pkgDir, "index"))
	}
	for _, t := range targets {
		for _, cand := range sourceFor(t) {
			if u := v.jsFileUnits(cand); len(u) > 0 {
				return u
			}
		}
	}
	return nil
}

// jsFileUnits returns the units of the module at p: p with any
// extension, or p/index.
func (v *View) jsFileUnits(p string) []string {
	return v.fileUnits([]string{stripExt(p), path.Join(p, "index")}, true)
}

// jsPackageName splits a bare spec into its package name (two segments
// when scoped) and subpath.
func jsPackageName(spec string) (name, sub string) {
	segs := strings.SplitN(spec, "/", 3)
	if strings.HasPrefix(spec, "@") && len(segs) >= 2 {
		name = segs[0] + "/" + segs[1]
		if len(segs) == 3 {
			sub = segs[2]
		}
		return name, sub
	}
	name, sub, _ = strings.Cut(spec, "/")
	return name, sub
}

// sourceFor maps a package entry to source candidates: itself, and with a
// build output directory (dist, lib, build, out) replaced by src.
func sourceFor(target string) []string {
	out := []string{target}
	for _, d := range []string{"/dist/", "/lib/", "/build/", "/out/"} {
		if i := strings.Index(target, d); i >= 0 {
			out = append(out, target[:i]+"/src/"+target[i+len(d):])
		}
	}
	return out
}

// matchAlias matches spec against pattern (exact, or with one "*") and
// returns target with the "*" part substituted.
func matchAlias(pattern, target, spec string) (string, bool) {
	star := strings.IndexByte(pattern, '*')
	if star < 0 {
		return target, pattern == spec
	}
	prefix, suffix := pattern[:star], pattern[star+1:]
	if !strings.HasPrefix(spec, prefix) || !strings.HasSuffix(spec, suffix) || len(spec) < len(prefix)+len(suffix) {
		return "", false
	}
	rest := spec[len(prefix) : len(spec)-len(suffix)]
	return strings.Replace(target, "*", rest, 1), true
}

// rustCrateUnits resolves use other_crate::a::b to a workspace crate
// declared in a Cargo.toml: its src directory (or the manifest's), then
// the path below it.
func (v *View) rustCrateUnits(segs []string) []string {
	dir, ok := v.project().named(manifest.KindCargo, strings.ReplaceAll(segs[0], "-", "_"))
	if !ok {
		return nil
	}
	root := path.Join(dir, "src")
	if _, ok := v.sn.Lookup(path.Join(root, "lib.rs")); !ok {
		if _, ok := v.sn.Lookup(path.Join(dir, "lib.rs")); ok {
			root = dir
		}
	}
	base := path.Join(append([]string{root}, segs[1:]...)...)
	var out []string
	for _, p := range []string{base, path.Dir(base)} {
		out = append(out, v.dirUnits(p, true)...)
		out = append(out, v.fileUnits([]string{p}, true)...)
		if p == root {
			break
		}
	}
	return uniq(out)
}

// phpUnits resolves a namespace or class spec through composer PSR-4
// (and PSR-0) prefixes, longest first.
func (v *View) phpUnits(spec string) []string {
	spec = strings.TrimPrefix(spec, "\\")
	best, bestDirs := "", []string(nil)
	for _, mfs := range v.project().byDir {
		for _, mf := range mfs {
			if mf.kind != manifest.KindComposer {
				continue
			}
			for _, imp := range mf.imports {
				if imp.Kind != facts.ImportAlias {
					continue
				}
				prefix := imp.Name
				if !strings.HasPrefix(spec+"\\", prefix) || len(prefix) < len(best) {
					continue
				}
				if len(prefix) > len(best) {
					best, bestDirs = prefix, nil
				}
				bestDirs = append(bestDirs, imp.Path)
			}
		}
	}
	var out []string
	rest := strings.ReplaceAll(strings.TrimPrefix(strings.TrimPrefix(spec+"\\", best), "\\"), "\\", "/")
	rest = strings.TrimSuffix(rest, "/")
	for _, d := range bestDirs {
		p := path.Join(d, rest)
		out = append(out, v.fileUnits([]string{p}, true)...)
		out = append(out, v.dirUnits(p, true)...)
		if len(out) == 0 && rest != "" {
			out = append(out, v.fileUnits([]string{path.Join(d, path.Dir(rest))}, true)...)
			out = append(out, v.dirUnits(path.Join(d, path.Dir(rest)), true)...)
		}
	}
	return uniq(out)
}

// includeUnits resolves a C/C++/Objective-C include through the include
// directories of compile_commands.json.
func (v *View) includeUnits(spec string) []string {
	var out []string
	for _, root := range v.project().includeRoots {
		out = append(out, v.fileUnits([]string{stripExt(path.Join(root, spec))}, true)...)
	}
	return uniq(out)
}

// dartPackageUnits resolves package:name/rest to lib/rest of the pubspec
// declaring name.
func (v *View) dartPackageUnits(spec string) ([]string, bool) {
	rel, ok := strings.CutPrefix(spec, "package:")
	if !ok {
		return nil, false
	}
	name, rest, _ := strings.Cut(rel, "/")
	dir, ok := v.project().named(manifest.KindPubspec, name)
	if !ok {
		return nil, false
	}
	return v.fileUnits([]string{stripExt(path.Join(dir, "lib", rest))}, true), true
}

// swiftModuleUnits resolves import Target to every unit under the SwiftPM
// target's directory.
func (v *View) swiftModuleUnits(spec string) []string {
	var out []string
	for root, name := range v.project().swiftTargets {
		if name != spec {
			continue
		}
		for _, pkg := range v.Packages() {
			if pkg == root || strings.HasPrefix(pkg, root+"/") {
				out = append(out, pkg)
			}
		}
	}
	return uniq(out)
}

// pythonRootUnits resolves a dotted module under the source roots of
// pyproject.toml (src layouts).
func (v *View) pythonRootUnits(rel string) []string {
	var out []string
	for _, mfs := range v.project().byDir {
		for _, mf := range mfs {
			if mf.kind != manifest.KindPyProject {
				continue
			}
			for _, imp := range mf.imports {
				if imp.Kind != facts.ImportRoot {
					continue
				}
				full := path.Join(imp.Path, rel)
				out = append(out, v.fileUnits([]string{full, path.Join(full, "__init__")}, true)...)
				out = append(out, v.dirUnits(full, true)...)
			}
		}
	}
	return uniq(out)
}
