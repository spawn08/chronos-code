package query

import (
	"cmp"
	"slices"
	"strings"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

// CallSite is one live call of a named function or method.
type CallSite struct {
	File      string
	Line, Col int
	Qualifier string
	QualKind  uint8
	Caller    *Symbol // enclosing declaration; nil for package-level initializers
}

// CallSites returns every live call whose callee name is name, in file and
// line order.
func (v *View) CallSites(name string) []CallSite {
	var out []CallSite
	for i := 0; i < v.sn.NumSegments(); i++ {
		seg := v.sn.Segment(i)
		lo, hi := seg.CallsTo(name)
		for k := lo; k < hi; k++ {
			if !v.sn.Live(i, seg.CallFile(k)) {
				continue
			}
			c := seg.Call(k)
			site := CallSite{File: v.meta(i, c.File).Path, Line: c.Line, Col: c.Col, Qualifier: c.Qualifier, QualKind: c.QualKind}
			if c.Caller >= 0 {
				s := v.symbol(i, c.Caller)
				site.Caller = &s
			}
			out = append(out, site)
		}
	}
	slices.SortFunc(out, func(a, b CallSite) int {
		if c := cmp.Compare(a.File, b.File); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Line, b.Line); c != 0 {
			return c
		}
		return cmp.Compare(a.Col, b.Col)
	})
	return out
}

// Callers maps each callee name to the sorted, distinct qualified identities
// ("Recv.Method" or "Func") of the declarations calling it. Recursive calls
// are skipped. Names without callers are absent.
func (v *View) Callers(names []string) map[string][]string {
	out := make(map[string][]string, len(names))
	for _, name := range names {
		seen := map[string]bool{}
		for i := 0; i < v.sn.NumSegments(); i++ {
			seg := v.sn.Segment(i)
			lo, hi := seg.CallsTo(name)
			for k := lo; k < hi; k++ {
				if !v.sn.Live(i, seg.CallFile(k)) {
					continue
				}
				caller := seg.CallCaller(k)
				if caller < 0 {
					continue
				}
				q := qualifiedOf(seg.SymbolReceiver(caller), seg.SymbolName(caller))
				if q == name || seen[q] {
					continue
				}
				seen[q] = true
				out[name] = append(out[name], q)
			}
		}
		slices.Sort(out[name])
	}
	return out
}

// qualifiedOf builds an owned Recv.Name identity from segment views.
func qualifiedOf(receiver, name string) string {
	if receiver == "" {
		return strings.Clone(name)
	}
	return facts.BaseType(receiver) + "." + name
}

// CallerSymbols returns the distinct declarations that call name, ordered by
// file, line and name.
func (v *View) CallerSymbols(name string) []Symbol {
	seen := map[uint64]bool{}
	var out []Symbol
	for _, site := range v.CallSites(name) {
		if site.Caller == nil || site.Caller.Qualified() == name || seen[site.Caller.ID] {
			continue
		}
		seen[site.Caller.ID] = true
		out = append(out, *site.Caller)
	}
	slices.SortFunc(out, func(a, b Symbol) int {
		if c := cmp.Compare(a.File, b.File); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Line, b.Line); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return out
}

// goBuiltins are predeclared functions and conversion types; an unqualified
// call to one is not a call into the workspace unless the package declares
// the same name.
var goBuiltins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true, "copy": true,
	"delete": true, "imag": true, "len": true, "make": true, "max": true, "min": true, "new": true,
	"panic": true, "print": true, "println": true, "real": true, "recover": true,
	"any": true, "bool": true, "byte": true, "complex64": true, "complex128": true, "error": true,
	"float32": true, "float64": true, "int": true, "int8": true, "int16": true, "int32": true,
	"int64": true, "rune": true, "string": true, "uint": true, "uint8": true, "uint16": true,
	"uint32": true, "uint64": true, "uintptr": true,
}

// Callees returns the sorted, distinct callee names called by the
// declarations with qualified identity q. Unqualified calls to Go builtins
// are omitted unless the caller's package declares that name.
func (v *View) Callees(q string) []string {
	short := q
	if dot := strings.LastIndexByte(q, '.'); dot >= 0 {
		short = q[dot+1:]
	}
	seen := map[string]bool{}
	v.eachNamed(short, func(i, k int) {
		s := v.symbol(i, k)
		if s.Qualified() != q || (s.Kind != facts.KindFunc && s.Kind != facts.KindMethod && s.Kind != facts.KindVar) {
			return
		}
		seg := v.sn.Segment(i)
		for _, ci := range seg.CallsFrom(k) {
			c := seg.Call(ci)
			if c.QualKind == facts.QualNone && goBuiltins[c.Callee] && !v.declaredIn(c.Callee, s.Package) {
				continue
			}
			if c.Callee != short || c.QualKind != facts.QualNone {
				seen[c.Callee] = true
			}
		}
	})
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func (v *View) declaredIn(name, pkg string) bool {
	found := false
	v.eachNamed(name, func(i, k int) {
		if !found && v.meta(i, v.sn.Segment(i).SymbolFile(k)).Package == pkg {
			found = true
		}
	})
	return found
}
