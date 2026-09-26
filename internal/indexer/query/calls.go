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
// line order. References of other kinds (type uses, extends, ...) are not
// calls and are skipped.
func (v *View) CallSites(name string) []CallSite {
	var out []CallSite
	for i := 0; i < v.sn.NumSegments(); i++ {
		seg := v.sn.Segment(i)
		lo, hi := seg.RefsTo(name)
		for k := lo; k < hi; k++ {
			if seg.RefKind(k) != facts.RefCall || !v.sn.Live(i, seg.RefFile(k)) {
				continue
			}
			c := seg.Ref(k)
			site := CallSite{File: v.meta(i, c.File).Path, Line: c.Line, Col: c.Col, Qualifier: c.Qualifier, QualKind: c.QualKind}
			if c.Enclosing >= 0 {
				s := v.symbol(i, c.Enclosing)
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

// Unresolved labels call sites of a callee name that has no indexed
// declaration (standard library, dependencies, generated or dynamic code).
// They are matched by the callee's name only.
const Unresolved = "unresolved"

// CallEdge is a resolved call or instantiation from one declaration to
// another: the first call site with the strongest label.
type CallEdge struct {
	Caller, Callee Symbol
	Line           int    // first call site with that label, in Caller.File
	Resolution     string // ImportResolved, TypeHinted, NameMatched, Ambiguous or Unresolved
	Candidates     int    // declarations the site could equally target (>= 1)
}

// labelRank orders resolution labels, strongest first.
var labelRank = map[string]int{TypeChecked: 0, ImportResolved: 1, TypeHinted: 2, NameMatched: 3, Ambiguous: 4, Unresolved: 5}

// IncomingEdges returns the call sites that resolve to any of targets, one
// edge per (caller, callee) pair, ordered by callee (in targets order),
// then caller file and line. Instantiations count only for constructors,
// which are reached through their class's name (new T()). Recursive calls
// are skipped.
func (v *View) IncomingEdges(targets []Symbol) []CallEdge {
	byName := map[string][]int{}
	var names []string
	for ti, t := range targets {
		name := t.Name
		if t.Kind == facts.KindConstructor && t.Receiver != "" {
			name = facts.BaseType(t.Receiver)
		}
		if _, ok := byName[name]; !ok {
			names = append(names, name)
		}
		byName[name] = append(byName[name], ti)
	}
	type key struct{ caller, callee uint64 }
	edges := map[key]*CallEdge{}
	order := map[uint64]int{}
	for ti, t := range targets {
		if _, ok := order[t.ID]; !ok {
			order[t.ID] = ti
		}
	}
	for _, name := range names {
		for i := 0; i < v.sn.NumSegments(); i++ {
			seg := v.sn.Segment(i)
			lo, hi := seg.RefsTo(name)
			for r := lo; r < hi; r++ {
				kind := seg.RefKind(r)
				if (kind != facts.RefCall && kind != facts.RefInstantiate) || !v.sn.Live(i, seg.RefFile(r)) {
					continue
				}
				caller := seg.RefEnclosing(r)
				if caller < 0 {
					continue
				}
				resolved, label := v.resolveRef(i, r)
				if len(resolved) == 0 {
					continue
				}
				for _, ti := range byName[name] {
					t := targets[ti]
					if kind == facts.RefInstantiate && t.Kind != facts.KindConstructor || !targetsInclude(resolved, t) {
						continue
					}
					cs := v.symbol(i, caller)
					if cs.ID == t.ID {
						continue
					}
					e := CallEdge{Caller: cs, Callee: t, Line: seg.Ref(r).Line, Resolution: label, Candidates: len(resolved)}
					k := key{cs.ID, t.ID}
					if old, ok := edges[k]; !ok || better(e, *old) {
						edges[k] = &e
					}
				}
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

// better reports whether e should replace old for the same pair: a
// stronger label, then an earlier site.
func better(e, old CallEdge) bool {
	if a, b := labelRank[e.Resolution], labelRank[old.Resolution]; a != b {
		return a < b
	}
	return e.Line < old.Line
}

func compareSites(a, b Symbol, la, lb int) int {
	if c := cmp.Compare(a.File, b.File); c != 0 {
		return c
	}
	if c := cmp.Compare(la, lb); c != 0 {
		return c
	}
	return cmp.Compare(a.Name, b.Name)
}

// CallerEdges returns the edges into every declaration Symbols(name, "")
// finds. When none is indexed, the callee is outside the workspace: the
// call sites of that callee name are returned instead, with Callee set to
// {Name: name} and the Unresolved label.
func (v *View) CallerEdges(name string) []CallEdge {
	if decls := v.Symbols(name, ""); len(decls) > 0 {
		return v.IncomingEdges(decls)
	}
	return v.unresolvedEdges(name)
}

func (v *View) unresolvedEdges(name string) []CallEdge {
	seen := map[uint64]bool{}
	var out []CallEdge
	for _, site := range v.CallSites(name) {
		if site.Caller == nil || site.Caller.Qualified() == name || seen[site.Caller.ID] {
			continue
		}
		seen[site.Caller.ID] = true
		out = append(out, CallEdge{Caller: *site.Caller, Callee: Symbol{Name: name}, Line: site.Line, Resolution: Unresolved, Candidates: 1})
	}
	return out
}

// Callers maps each callee name to the sorted, distinct qualified identities
// ("Recv.Method" or "Func") of the declarations calling it, as CallerEdges
// resolves them. Names without callers are absent.
func (v *View) Callers(names []string) map[string][]string {
	out := make(map[string][]string, len(names))
	for _, name := range names {
		seen := map[string]bool{}
		for _, e := range v.CallerEdges(name) {
			q := e.Caller.Qualified()
			if q == name || seen[q] {
				continue
			}
			seen[q] = true
			out[name] = append(out[name], q)
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

// CallerSymbols returns the distinct declarations calling name, as
// CallerEdges resolves them, ordered by file, line and name.
func (v *View) CallerSymbols(name string) []Symbol {
	seen := map[uint64]bool{}
	var out []Symbol
	for _, e := range v.CallerEdges(name) {
		if e.Caller.Qualified() == name || seen[e.Caller.ID] {
			continue
		}
		seen[e.Caller.ID] = true
		out = append(out, e.Caller)
	}
	slices.SortFunc(out, func(a, b Symbol) int { return compareSites(a, b, a.Line, b.Line) })
	return out
}

// OutgoingEdges returns the calls made inside each of callers that resolve
// to workspace declarations, one edge per (caller,
// callee) pair, in callers order and then call-site order. Calls that
// resolve to nothing (external code) are omitted.
func (v *View) OutgoingEdges(callers []Symbol) []CallEdge {
	var out []CallEdge
	for _, c := range callers {
		at := map[uint64]int{}
		first := len(out)
		for _, call := range v.Outgoing(c) {
			if call.Kind != facts.RefCall {
				continue
			}
			targets, label := v.Resolve(call)
			for _, t := range targets {
				if t.ID == c.ID {
					continue
				}
				e := CallEdge{Caller: c, Callee: t, Line: call.Line, Resolution: label, Candidates: len(targets)}
				if j, ok := at[t.ID]; ok {
					if better(e, out[j]) {
						out[j] = e
					}
					continue
				}
				at[t.ID] = len(out)
				out = append(out, e)
			}
		}
		slices.SortStableFunc(out[first:], func(a, b CallEdge) int { return cmp.Compare(a.Line, b.Line) })
	}
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

// Callees returns the sorted, distinct qualified identities of the
// workspace declarations called by the declarations with qualified
// identity q, as OutgoingEdges resolves them. Calls into external code are
// omitted.
func (v *View) Callees(q string) []string {
	short := q
	if dot := strings.LastIndexByte(q, '.'); dot >= 0 {
		short = q[dot+1:]
	}
	var decls []Symbol
	v.eachNamed(short, func(i, k int) {
		s := v.symbol(i, k)
		switch s.Kind {
		case facts.KindFunc, facts.KindMethod, facts.KindVar, facts.KindConstructor:
			if s.Qualified() == q {
				decls = append(decls, s)
			}
		}
	})
	seen := map[string]bool{}
	for _, e := range v.OutgoingEdges(decls) {
		seen[e.Callee.Qualified()] = true
	}
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
