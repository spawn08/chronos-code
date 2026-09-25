// Package query answers symbol, call, package and search queries over one
// index snapshot. Call sites are stored unresolved; everything here resolves
// names against the snapshot's live symbol table at query time, so results
// always reflect exactly one generation.
//
// Every relationship answered here is syntactic: callers are matched by the
// callee's name and implementations by method names. Results say so through
// the Resolution labels; the precise tier (M4) upgrades them later.
package query

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/cespare/xxhash/v2"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/segment"
	"github.com/spawn08/chronos-code/internal/indexer/store"
)

// Resolution labels, strongest first. See docs/chronos-indexer.md.
const (
	TypeChecked    = "type_checked"
	ImportResolved = "import_resolved"
	NameMatched    = "name_matched"
	Ambiguous      = "ambiguous"
)

// Symbol is one declaration, with owned strings.
type Symbol struct {
	ID        uint64 // stable for the same declaration at the same place
	Name      string
	Kind      string
	Receiver  string
	Signature string
	Doc       string
	Package   string // import path (Go) or directory
	PkgName   string
	File      string // root-relative, slash-separated
	Lang      string
	Line      int
	EndLine   int
	Exported  bool
	Parent    string // Name of the enclosing symbol, if any (interface of a method spec)
}

// Qualified returns Recv.Name for methods and Name otherwise, the identity
// callers are recorded under.
func (s Symbol) Qualified() string {
	if s.Receiver == "" {
		return s.Name
	}
	return facts.BaseType(s.Receiver) + "." + s.Name
}

// View is a read-only query view over one snapshot. It must not be used after
// the snapshot is released.
type View struct {
	sn      *store.Snapshot
	cache   *Cache
	metas   map[fileKey]segment.FileMeta
	visible map[string]map[string]bool // package -> itself and its imports
	syms    map[fileKey]Symbol         // decoded symbols by (segment, symbol index)
}

type fileKey struct{ seg, file int }

// NewView returns a view over sn. cache may be nil; pass the engine's shared
// cache so segment search indexes are built once per segment.
func NewView(sn *store.Snapshot, cache *Cache) *View {
	if cache == nil {
		cache = NewCache()
	}
	return &View{sn: sn, cache: cache, metas: map[fileKey]segment.FileMeta{}}
}

// Generation returns the snapshot generation.
func (v *View) Generation() uint64 { return v.sn.Generation() }

// NumFiles returns the number of live files.
func (v *View) NumFiles() int { return v.sn.NumFiles() }

func (v *View) meta(seg, file int) segment.FileMeta {
	k := fileKey{seg, file}
	if m, ok := v.metas[k]; ok {
		return m
	}
	m := v.sn.Segment(seg).FileMeta(file)
	v.metas[k] = m
	return m
}

// symbol decodes symbol k of segment i, once per view.
func (v *View) symbol(i, k int) Symbol {
	key := fileKey{i, k}
	if s, ok := v.syms[key]; ok {
		return s
	}
	s := v.decodeSymbol(i, k)
	if v.syms == nil {
		v.syms = map[fileKey]Symbol{}
	}
	v.syms[key] = s
	return s
}

func (v *View) decodeSymbol(i, k int) Symbol {
	seg := v.sn.Segment(i)
	rec := seg.Symbol(k)
	m := v.meta(i, rec.File)
	s := Symbol{
		Name: rec.Name, Kind: rec.Kind, Receiver: rec.Receiver, Signature: rec.Signature, Doc: rec.Doc,
		Package: m.Package, PkgName: m.PkgName, File: m.Path, Lang: m.Lang,
		Line: rec.Line, EndLine: rec.EndLine, Exported: rec.Exported,
	}
	if rec.Parent >= 0 {
		s.Parent = strings.Clone(seg.SymbolName(rec.Parent))
	}
	s.ID = symbolID(s)
	return s
}

func symbolID(s Symbol) uint64 {
	var b strings.Builder
	b.Grow(len(s.File) + len(s.Name) + len(s.Receiver) + len(s.Kind) + 16)
	b.WriteString(s.File)
	b.WriteByte(0)
	b.WriteString(s.Receiver)
	b.WriteByte(0)
	b.WriteString(s.Name)
	b.WriteByte(0)
	b.WriteString(s.Kind)
	b.WriteByte(0)
	b.WriteString(strconv.Itoa(s.Line))
	return xxhash.Sum64String(b.String())
}

// eachNamed calls fn for every live symbol named name, across segments.
func (v *View) eachNamed(name string, fn func(i, k int)) {
	for i := 0; i < v.sn.NumSegments(); i++ {
		seg := v.sn.Segment(i)
		lo, hi := seg.SymbolsNamed(name)
		for k := lo; k < hi; k++ {
			if v.sn.Live(i, seg.SymbolFile(k)) {
				fn(i, k)
			}
		}
	}
}

// Symbols returns the declarations named name, optionally filtered by kind.
// A dotted name ("Engine.Update", "store.Open") also matches methods by
// receiver type and declarations by package name. Embeds are never returned.
// Results are ordered by package, file and line.
func (v *View) Symbols(name, kind string) []Symbol {
	var out []Symbol
	add := func(s Symbol) {
		if s.Kind != facts.KindEmbed && (kind == "" || s.Kind == kind) {
			out = append(out, s)
		}
	}
	v.eachNamed(name, func(i, k int) { add(v.symbol(i, k)) })
	if len(out) == 0 {
		if dot := strings.LastIndexByte(name, '.'); dot > 0 && dot < len(name)-1 {
			left, short := name[:dot], name[dot+1:]
			v.eachNamed(short, func(i, k int) {
				s := v.symbol(i, k)
				if (s.Receiver != "" && facts.BaseType(s.Receiver) == left) ||
					(s.Receiver == "" && (s.PkgName == left || s.Package == left || strings.HasSuffix(s.Package, "/"+left))) {
					add(s)
				}
			})
		}
	}
	sortSymbols(out)
	return out
}

func sortSymbols(out []Symbol) {
	slices.SortFunc(out, func(a, b Symbol) int {
		if c := cmp.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		if c := cmp.Compare(a.File, b.File); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Line, b.Line); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
}

// ByQualified maps qualified identities ("Recv.Method" or "Func") to their
// declarations.
func (v *View) ByQualified(names []string) map[string][]Symbol {
	out := make(map[string][]Symbol, len(names))
	for _, q := range names {
		short := q
		if dot := strings.LastIndexByte(q, '.'); dot >= 0 {
			short = q[dot+1:]
		}
		v.eachNamed(short, func(i, k int) {
			s := v.symbol(i, k)
			if s.Kind != facts.KindEmbed && s.Qualified() == q {
				out[q] = append(out[q], s)
			}
		})
		sortSymbols(out[q])
	}
	return out
}

// FileSymbols returns the declarations in path, in line order.
func (v *View) FileSymbols(path string) []Symbol {
	ref, ok := v.sn.Lookup(path)
	if !ok {
		return nil
	}
	i, f := int(ref.Seg), int(ref.File)
	seg := v.sn.Segment(i)
	var out []Symbol
	for _, k := range seg.SymbolsInFile(f) {
		if seg.SymbolKind(k) != facts.KindEmbed {
			out = append(out, v.symbol(i, k))
		}
	}
	return out
}

// SymbolsInRange returns the declarations of path overlapping [start, end].
func (v *View) SymbolsInRange(path string, start, end int) []Symbol {
	var out []Symbol
	for _, s := range v.FileSymbols(path) {
		if s.Line <= end && max(s.Line, s.EndLine) >= start {
			out = append(out, s)
		}
	}
	return out
}

// FileMeta returns the indexed metadata of a live file.
func (v *View) FileMeta(path string) (segment.FileMeta, bool) { return v.sn.Meta(path) }

// File is a live file with its package.
type File struct {
	Path    string
	Package string
}

// Packages returns every package with at least one live file, sorted.
func (v *View) Packages() []string { return v.cache.packageNames(v.sn) }

// packagePaths returns the live paths of pkg, sorted.
func (v *View) packagePaths(pkg string) []string {
	var out []string
	for i := 0; i < v.sn.NumSegments(); i++ {
		seg := v.sn.Segment(i)
		for _, f := range seg.PackageFiles(pkg) {
			if v.sn.Live(i, f) {
				out = append(out, seg.FilePath(f))
			}
		}
	}
	slices.Sort(out)
	return out
}

// PackageFiles returns the live files of pkg, sorted by path.
func (v *View) PackageFiles(pkg string) []File {
	paths := v.packagePaths(pkg)
	out := make([]File, len(paths))
	for i, p := range paths {
		out[i] = File{Path: p, Package: pkg}
	}
	return out
}

// PackageSymbols returns the declarations of pkg, ordered by file and line.
func (v *View) PackageSymbols(pkg string) []Symbol {
	var out []Symbol
	for _, p := range v.packagePaths(pkg) {
		out = append(out, v.FileSymbols(p)...)
	}
	return out
}

// PackageImports returns the distinct import paths of pkg's files, sorted.
func (v *View) PackageImports(pkg string) []string {
	seen := map[string]bool{}
	for _, p := range v.packagePaths(pkg) {
		ref, ok := v.sn.Lookup(p)
		if !ok {
			continue
		}
		seg := v.sn.Segment(int(ref.Seg))
		for _, ii := range seg.ImportsInFile(int(ref.File)) {
			seen[seg.Import(ii).Path] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// PackageDeps returns the indexed packages pkg imports, sorted.
func (v *View) PackageDeps(pkg string) []string {
	var out []string
	for _, imp := range v.PackageImports(pkg) {
		if imp != pkg && len(v.packagePaths(imp)) > 0 {
			out = append(out, imp)
		}
	}
	return out
}

// PackageDependents returns the indexed packages that import pkg, sorted.
func (v *View) PackageDependents(pkg string) []string {
	seen := map[string]bool{}
	for i := 0; i < v.sn.NumSegments(); i++ {
		seg := v.sn.Segment(i)
		for _, ii := range seg.Importers(pkg) {
			imp := seg.Import(ii)
			if v.sn.Live(i, imp.File) {
				if p := v.meta(i, imp.File).Package; p != pkg {
					seen[p] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// Stats counts live records.
type Stats struct {
	Files, Packages, Symbols, Calls int
}

// Stats returns live record counts.
func (v *View) Stats() Stats {
	decls, calls := liveCounts(v.sn)
	return Stats{Files: v.sn.NumFiles(), Packages: len(v.Packages()), Symbols: decls, Calls: calls}
}
