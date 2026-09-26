package query

import (
	"slices"
	"strings"

	"github.com/spawn08/chronos-code/indexer/facts"
)

// wellKnown gives the method sets of common standard-library interfaces, so
// an interface embedding them (io.ReadCloser-style) keeps its full method
// set even though the standard library is not indexed.
var wellKnown = map[string][]string{
	"error": {"Error"}, "fmt.Stringer": {"String"}, "fmt.GoStringer": {"GoString"},
	"io.Reader": {"Read"}, "io.Writer": {"Write"}, "io.Closer": {"Close"}, "io.Seeker": {"Seek"},
	"io.ReaderAt": {"ReadAt"}, "io.WriterAt": {"WriteAt"}, "io.ByteReader": {"ReadByte"},
	"io.ByteWriter": {"WriteByte"}, "io.RuneReader": {"ReadRune"}, "io.StringWriter": {"WriteString"},
	"io.ReaderFrom": {"ReadFrom"}, "io.WriterTo": {"WriteTo"},
	"io.ReadWriter": {"Read", "Write"}, "io.ReadCloser": {"Read", "Close"}, "io.WriteCloser": {"Write", "Close"},
	"io.ReadWriteCloser": {"Read", "Write", "Close"}, "io.ReadSeeker": {"Read", "Seek"},
	"sort.Interface": {"Len", "Less", "Swap"}, "context.Context": {"Deadline", "Done", "Err", "Value"},
	"json.Marshaler": {"MarshalJSON"}, "json.Unmarshaler": {"UnmarshalJSON"},
	"encoding.TextMarshaler": {"MarshalText"}, "encoding.TextUnmarshaler": {"UnmarshalText"},
	"http.Handler": {"ServeHTTP"}, "heap.Interface": {"Len", "Less", "Swap", "Push", "Pop"},
}

// Implementations returns the sorted short names of the types
// implementing an interface (or extending a type) named iface:
//
//   - Go: concrete types whose method names cover every method of the
//     interface, including methods promoted from embedded fields declared
//     in the same package. The match is by method name only (Resolution
//     NameMatched): signatures are not compared. Interfaces without methods
//     are skipped, since every type satisfies them.
//   - Other languages: types whose extends or implements clauses resolve
//     to it (see Subtypes), transitively; interfaces, traits and protocols
//     in the chain are followed but not listed.
func (v *View) Implementations(iface string) []string {
	found := map[string]bool{}
	for _, s := range v.Symbols(iface, "") {
		if s.Name == iface && s.Lang != "go" && isTypeKind(s.Kind) {
			v.collectSubtypes(s, found)
		}
	}
	for _, it := range v.Symbols(iface, facts.KindInterface) {
		if it.Name != iface {
			continue
		}
		methods := v.interfaceMethods(it, map[uint64]bool{}, 0)
		if len(methods) == 0 {
			continue
		}
		for _, cand := range v.candidates(methods) {
			if cand.name == iface && cand.pkg == it.Package {
				continue
			}
			if covers(v.methodSet(cand.pkg, cand.name, 0), methods) {
				found[cand.name] = true
			}
		}
	}
	out := make([]string, 0, len(found))
	for n := range found {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// maxSubtypeDepth bounds the inheritance chain Implementations follows.
const maxSubtypeDepth = 6

func (v *View) collectSubtypes(root Symbol, found map[string]bool) {
	visited := map[uint64]bool{root.ID: true}
	frontier := []Symbol{root}
	for d := 0; d < maxSubtypeDepth && len(frontier) > 0; d++ {
		var next []Symbol
		for _, t := range frontier {
			for _, sub := range v.Subtypes(t) {
				if visited[sub.Caller.ID] {
					continue
				}
				visited[sub.Caller.ID] = true
				next = append(next, sub.Caller)
				switch sub.Caller.Kind {
				case facts.KindInterface, facts.KindTrait, facts.KindProtocol:
				default:
					found[sub.Caller.Name] = true
				}
			}
		}
		frontier = next
	}
}

// Subtypes returns the type declarations whose extends or implements
// clauses name s and resolve to it, with the label of each, in file and
// line order. Caller is the subtype.
func (v *View) Subtypes(s Symbol) []Incoming {
	var out []Incoming
	for i := 0; i < v.sn.NumSegments(); i++ {
		seg := v.sn.Segment(i)
		lo, hi := seg.RefsTo(s.Name)
		for r := lo; r < hi; r++ {
			kind := seg.RefKind(r)
			if (kind != facts.RefExtends && kind != facts.RefImplements) || !v.sn.Live(i, seg.RefFile(r)) {
				continue
			}
			enc := seg.RefEnclosing(r)
			if enc < 0 {
				continue
			}
			sub := v.symbol(i, enc)
			if sub.ID == s.ID || !isTypeKind(sub.Kind) {
				continue
			}
			targets, label := v.resolveRef(i, r)
			if !targetsInclude(targets, s) {
				continue
			}
			out = append(out, Incoming{Caller: sub, Line: seg.Ref(r).Line, Resolution: label, Candidates: max(1, len(targets))})
		}
	}
	slices.SortFunc(out, func(a, b Incoming) int { return compareSites(a.Caller, b.Caller, a.Line, b.Line) })
	return out
}

// interfaceMethods returns the method names of an interface declaration,
// following embedded interfaces declared in the index or known above.
func (v *View) interfaceMethods(it Symbol, visited map[uint64]bool, depth int) map[string]bool {
	out := map[string]bool{}
	if visited[it.ID] || depth > 8 {
		return out
	}
	visited[it.ID] = true
	ref, ok := v.sn.Lookup(it.File)
	if !ok {
		return out
	}
	i, f := int(ref.Seg), int(ref.File)
	seg := v.sn.Segment(i)
	self := -1
	for _, k := range seg.SymbolsInFile(f) {
		rec := seg.Symbol(k)
		if rec.Name == it.Name && rec.Line == it.Line && rec.Kind == facts.KindInterface {
			self = k
			break
		}
	}
	if self < 0 {
		return out
	}
	for _, k := range seg.SymbolsInFile(f) {
		if seg.SymbolParent(k) != self {
			continue
		}
		rec := seg.Symbol(k)
		switch rec.Kind {
		case facts.KindMethod:
			out[rec.Name] = true
		case facts.KindEmbed:
			if known, ok := wellKnown[rec.Signature]; ok {
				for _, m := range known {
					out[m] = true
				}
				continue
			}
			for _, emb := range v.resolveType(rec.Name, rec.Signature, it.Package, facts.KindInterface) {
				for m := range v.interfaceMethods(emb, visited, depth+1) {
					out[m] = true
				}
			}
		}
	}
	return out
}

// resolveType finds declarations of a type referenced by expr ("T", "pkg.T",
// "*pkg.T[X]") from package from: a qualified reference matches by package
// name, an unqualified one by the same package.
func (v *View) resolveType(name, expr, from, kind string) []Symbol {
	qual := ""
	e := strings.TrimLeft(expr, "*")
	if i := strings.IndexByte(e, '['); i >= 0 {
		e = e[:i]
	}
	if dot := strings.LastIndexByte(e, '.'); dot > 0 {
		qual = e[:dot]
	}
	var out []Symbol
	for _, s := range v.Symbols(name, kind) {
		if s.Name != name {
			continue
		}
		if (qual == "" && s.Package == from) || (qual != "" && (s.PkgName == qual || strings.HasSuffix(s.Package, "/"+qual))) {
			out = append(out, s)
		}
	}
	return out
}

type typeRef struct{ pkg, name string }

// candidates returns the concrete types declaring a method with the rarest
// of the required names; only they can cover the whole set.
func (v *View) candidates(methods map[string]bool) []typeRef {
	rarest, best := "", -1
	for m := range methods {
		n := 0
		v.eachNamed(m, func(int, int) { n++ })
		if best < 0 || n < best || (n == best && m < rarest) {
			rarest, best = m, n
		}
	}
	seen := map[typeRef]bool{}
	var out []typeRef
	add := func(r typeRef) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	v.eachNamed(rarest, func(i, k int) {
		s := v.symbol(i, k)
		if s.Kind == facts.KindMethod && s.Parent == "" && s.Receiver != "" {
			add(typeRef{s.Package, facts.BaseType(s.Receiver)})
		}
	})
	// Types that only get the method by embedding a type that declares it.
	for _, r := range slices.Clone(out) {
		for _, emb := range v.embeddersOf(r) {
			add(emb)
		}
	}
	return out
}

// embeddersOf returns struct types in the index that embed r.
func (v *View) embeddersOf(r typeRef) []typeRef {
	var out []typeRef
	v.eachNamed(r.name, func(i, k int) {
		seg := v.sn.Segment(i)
		if seg.SymbolKind(k) != facts.KindEmbed {
			return
		}
		parent := seg.SymbolParent(k)
		if parent < 0 || seg.SymbolKind(parent) != facts.KindStruct {
			return
		}
		pkg := v.meta(i, seg.SymbolFile(k)).Package
		out = append(out, typeRef{pkg, strings.Clone(seg.SymbolName(parent))})
	})
	return out
}

// methodSet returns the method names declared on type name in pkg, plus
// those promoted from its embedded fields that resolve in the index.
func (v *View) methodSet(pkg, name string, depth int) map[string]bool {
	out := map[string]bool{}
	for _, p := range v.packagePaths(pkg) {
		ref, ok := v.sn.Lookup(p)
		if !ok {
			continue
		}
		i, f := int(ref.Seg), int(ref.File)
		seg := v.sn.Segment(i)
		for _, k := range seg.SymbolsInFile(f) {
			rec := seg.Symbol(k)
			switch {
			case rec.Kind == facts.KindMethod && rec.Parent < 0 && facts.BaseType(rec.Receiver) == name:
				out[rec.Name] = true
			case rec.Kind == facts.KindEmbed && depth < 4 && rec.Parent >= 0 && seg.SymbolName(rec.Parent) == name &&
				seg.SymbolKind(rec.Parent) == facts.KindStruct:
				if known, ok := wellKnown[strings.TrimLeft(rec.Signature, "*")]; ok {
					for _, m := range known {
						out[m] = true
					}
				}
				for _, t := range v.resolveType(rec.Name, rec.Signature, pkg, "") {
					if t.Kind == facts.KindInterface {
						for m := range v.interfaceMethods(t, map[uint64]bool{}, 0) {
							out[m] = true
						}
						continue
					}
					for m := range v.methodSet(t.Package, t.Name, depth+1) {
						out[m] = true
					}
				}
			}
		}
	}
	return out
}

func covers(have, want map[string]bool) bool {
	for m := range want {
		if !have[m] {
			return false
		}
	}
	return true
}
