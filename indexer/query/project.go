package query

import (
	"path"
	"slices"
	"strings"

	"github.com/spawn08/chronos-code/indexer/extract/manifest"
	"github.com/spawn08/chronos-code/indexer/facts"
	"github.com/spawn08/chronos-code/indexer/segment"
	"github.com/spawn08/chronos-code/indexer/store"
)

// project is what the manifests of one generation say about the
// repository's modules and build units (see package extract/manifest).
// It is built once per generation from the few manifest files and is
// read-only afterwards.
type project struct {
	byDir  map[string][]*manifestFile // directory -> manifests in it
	byName map[string]map[string]string
	// gradle project paths (":a:b") -> directory, from settings scripts
	gradle map[string]string
	// SwiftPM target directories -> target name
	swiftTargets map[string]string
	// include directories from compile_commands.json
	includeRoots []string
	// declared packages of package-scoped languages (Java, Kotlin, Scala,
	// C#), including files that are no longer live
	declared map[string]bool
}

type manifestFile struct {
	kind, path, dir, name string
	imports               []facts.Import
	exports               []facts.Export
}

// buildKinds are the manifests that make their directory a build unit.
var buildKinds = map[string]bool{
	manifest.KindNPM: true, manifest.KindCargo: true, manifest.KindMaven: true, manifest.KindGradle: true,
	manifest.KindBazel: true, manifest.KindCSProj: true, manifest.KindPubspec: true,
}

// packageScoped are the languages whose declared package (namespace) is a
// unit of visibility independent of directories.
var packageScoped = map[string]bool{"java": true, "kotlin": true, "scala": true, "csharp": true}

func (c *Cache) project(sn *store.Snapshot) *project {
	c.mu.Lock()
	if c.proj != nil && c.proj.gen == sn.Generation() {
		p := c.proj.p
		c.mu.Unlock()
		return p
	}
	c.mu.Unlock()
	p := &project{
		byDir: map[string][]*manifestFile{}, byName: map[string]map[string]string{},
		gradle: map[string]string{}, swiftTargets: map[string]string{}, declared: map[string]bool{},
	}
	for i := 0; i < sn.NumSegments(); i++ {
		seg := sn.Segment(i)
		idx := c.segIndex(sn, seg)
		for name := range idx.declared {
			p.declared[name] = true
		}
		for _, f := range idx.manifests {
			if !sn.Live(i, int(f)) {
				continue
			}
			m := seg.FileMeta(int(f))
			mf := &manifestFile{kind: m.Lang, path: m.Path, dir: path.Dir(m.Path), name: m.PkgName, exports: seg.Exports(int(f))}
			for _, ii := range seg.ImportsInFile(int(f)) {
				mf.imports = append(mf.imports, seg.Import(ii).Import)
			}
			p.add(mf)
		}
	}
	for dir := range p.byDir {
		slices.SortFunc(p.byDir[dir], func(a, b *manifestFile) int { return strings.Compare(a.path, b.path) })
	}
	slices.Sort(p.includeRoots)
	p.includeRoots = slices.Compact(p.includeRoots)
	c.mu.Lock()
	c.proj = &projMemo{gen: sn.Generation(), p: p}
	c.mu.Unlock()
	return p
}

func (p *project) add(mf *manifestFile) {
	p.byDir[mf.dir] = append(p.byDir[mf.dir], mf)
	if mf.name != "" {
		key := mf.kind
		if p.byName[key] == nil {
			p.byName[key] = map[string]string{}
		}
		name := mf.name
		if mf.kind == manifest.KindCargo {
			name = strings.ReplaceAll(name, "-", "_")
		}
		if _, dup := p.byName[key][name]; !dup || mf.dir < p.byName[key][name] {
			p.byName[key][name] = mf.dir
		}
	}
	for _, imp := range mf.imports {
		if imp.Kind != facts.ImportRoot {
			continue
		}
		switch mf.kind {
		case manifest.KindGradle:
			if imp.Name != "" {
				p.gradle[imp.Name] = imp.Path
			}
		case manifest.KindSwiftPM:
			p.swiftTargets[imp.Path] = imp.Name
		case manifest.KindCompDB:
			p.includeRoots = append(p.includeRoots, imp.Path)
		}
	}
}

// named returns the directory of the manifest of kind declaring name.
func (p *project) named(kind, name string) (string, bool) {
	dir, ok := p.byName[kind][name]
	return dir, ok
}

// nearest returns the manifest of kind in dir or the closest directory
// above it, if any.
func (p *project) nearest(dir, kind string) *manifestFile {
	for d := dir; ; d = path.Dir(d) {
		for _, mf := range p.byDir[d] {
			if mf.kind == kind {
				return mf
			}
		}
		if d == "." || d == "/" || d == "" {
			return nil
		}
	}
}

// file returns the manifest at a root-relative path.
func (p *project) file(rel string) *manifestFile {
	for _, mf := range p.byDir[path.Dir(rel)] {
		if mf.path == rel {
			return mf
		}
	}
	return nil
}

// buildUnit returns the build unit containing dir: the closest directory
// at or above it with a build manifest, or the SwiftPM target root.
func (p *project) buildUnit(dir string) (string, bool) {
	for d := dir; ; d = path.Dir(d) {
		if _, ok := p.swiftTargets[d]; ok {
			return d, true
		}
		for _, mf := range p.byDir[d] {
			if buildKinds[mf.kind] && !(mf.kind == manifest.KindGradle && strings.HasPrefix(path.Base(mf.path), "settings.")) &&
				!(mf.kind == manifest.KindNPM && path.Base(mf.path) != "package.json") {
				return d, true
			}
		}
		if d == "." || d == "/" || d == "" {
			return "", false
		}
	}
}

// deps returns the build units unit depends on directly, as the manifests
// in its directory declare them. known is false when no manifest there
// can declare workspace dependencies.
func (p *project) deps(unit string) (out []string, known bool) {
	for _, mf := range p.byDir[unit] {
		if !buildKinds[mf.kind] {
			continue
		}
		known = true
		for _, imp := range mf.imports {
			if imp.Kind != facts.ImportDepend {
				continue
			}
			dir := imp.Path
			if dir == "" {
				dir = p.depDir(mf.kind, imp.Name, unit)
			}
			if dir == "" {
				continue
			}
			if u, ok := p.buildUnit(dir); ok && u != unit {
				out = append(out, u)
			}
		}
	}
	return out, known
}

// depDir maps a dependency named in a manifest of kind to the directory of
// the workspace unit with that name, if any.
func (p *project) depDir(kind, name, from string) string {
	switch kind {
	case manifest.KindGradle:
		if dir, ok := p.gradle[name]; ok {
			return dir
		}
		// Default layout: :a:b is a/b under the root project.
		root := "."
		for d := from; ; d = path.Dir(d) {
			if mf := p.nearest(d, manifest.KindGradle); mf != nil && strings.HasPrefix(path.Base(mf.path), "settings.") {
				root = mf.dir
				break
			}
			if d == "." || d == "/" || d == "" {
				break
			}
		}
		return path.Join(root, strings.ReplaceAll(strings.TrimPrefix(name, ":"), ":", "/"))
	case manifest.KindCargo:
		name = strings.ReplaceAll(name, "-", "_")
	}
	if dir, ok := p.named(kind, name); ok {
		return dir
	}
	return ""
}

// depClosure returns unit and every build unit it depends on,
// transitively (bounded), and whether the unit declares its dependencies.
func (p *project) depClosure(unit string) ([]string, bool) {
	direct, known := p.deps(unit)
	if !known {
		return nil, false
	}
	seen := map[string]bool{unit: true}
	out := []string{unit}
	queue := direct
	for len(queue) > 0 && len(out) < 512 {
		u := queue[0]
		queue = queue[1:]
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
		next, _ := p.deps(u)
		queue = append(queue, next...)
	}
	slices.Sort(out)
	return out, true
}

// swiftTarget returns the SwiftPM target root containing dir.
func (p *project) swiftTarget(dir string) (string, bool) {
	for d := dir; ; d = path.Dir(d) {
		if _, ok := p.swiftTargets[d]; ok {
			return d, true
		}
		if d == "." || d == "/" || d == "" {
			return "", false
		}
	}
}

// segIndex holds per-segment indexes the project needs: the manifest
// files and the declared packages of package-scoped languages. Segments
// are immutable, so each is built once.
type segIndex struct {
	manifests []int32
	declared  map[string]bool
}

func (c *Cache) segIndex(sn *store.Snapshot, seg *segment.Segment) *segIndex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if idx, ok := c.segs[seg]; ok {
		return idx
	}
	if c.segs == nil {
		c.segs = map[*segment.Segment]*segIndex{}
	}
	live := map[*segment.Segment]bool{}
	for i := 0; i < sn.NumSegments(); i++ {
		live[sn.Segment(i)] = true
	}
	for s := range c.segs {
		if !live[s] {
			delete(c.segs, s)
		}
	}
	idx := &segIndex{declared: map[string]bool{}}
	for f := 0; f < seg.NumFiles(); f++ {
		lang := seg.FileLangView(f)
		switch {
		case manifest.IsManifest(lang):
			idx.manifests = append(idx.manifests, int32(f))
		case packageScoped[lang]:
			if pkg := seg.FilePkgNameView(f); pkg != "" && !idx.declared[pkg] {
				idx.declared[strings.Clone(pkg)] = true
			}
		}
	}
	c.segs[seg] = idx
	return idx
}

type projMemo struct {
	gen uint64
	p   *project
}

// project returns the view's project model.
func (v *View) project() *project { return v.cache.project(v.sn) }

// --- resolution helpers ---

// inBuildDeps keeps the candidates declared in the caller's build unit or
// the units it depends on, when the caller's unit declares its
// dependencies (Bazel, Maven, Gradle, Cargo, npm, .csproj, pubspec) and
// some candidate is inside them; otherwise cands is returned unchanged.
func (v *View) inBuildDeps(fc *fileCtx, cands []Symbol) []Symbol {
	if len(cands) < 2 || fc.meta.Lang == "go" {
		return cands
	}
	closure := v.buildClosure(path.Dir(fc.meta.Path))
	if closure == nil {
		return cands
	}
	p := v.project()
	in := filter(cands, func(s Symbol) bool {
		u, ok := p.buildUnit(path.Dir(s.File))
		_, found := slices.BinarySearch(closure, u)
		return ok && found
	})
	if len(in) == 0 {
		return cands
	}
	return in
}

// buildClosure returns the sorted build units visible from files in dir
// (its unit and its dependencies), or nil when unknown. Memoized per
// generation.
func (v *View) buildClosure(dir string) []string {
	out := v.cache.memoUnits(v.sn.Generation(), "build\x00"+dir, func() []string {
		p := v.project()
		unit, ok := p.buildUnit(dir)
		if !ok {
			return []string{}
		}
		closure, known := p.depClosure(unit)
		if !known {
			return []string{}
		}
		return closure
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

// sameTarget reports whether two directories are in one SwiftPM target.
func (v *View) sameTarget(a, b string) bool {
	p := v.project()
	ta, ok := p.swiftTarget(a)
	if !ok {
		return false
	}
	tb, ok := p.swiftTarget(b)
	return ok && ta == tb
}
