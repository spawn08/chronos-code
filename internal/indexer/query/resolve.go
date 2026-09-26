package query

import (
	"strings"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

// maxCandidates bounds the targets returned for one ambiguous call.
const maxCandidates = 8

// OutCall is one call made inside a declaration.
type OutCall struct {
	Callee, Qualifier string
	QualKind          uint8
	Line              int
}

// Outgoing returns the calls made inside declaration s, in line order.
func (v *View) Outgoing(s Symbol) []OutCall {
	ref, ok := v.sn.Lookup(s.File)
	if !ok {
		return nil
	}
	i, f := int(ref.Seg), int(ref.File)
	seg := v.sn.Segment(i)
	for _, k := range seg.SymbolsInFile(f) {
		rec := seg.Symbol(k)
		if rec.Name != s.Name || rec.Line != s.Line || rec.Kind != s.Kind {
			continue
		}
		var out []OutCall
		for _, ci := range seg.RefsFrom(k) {
			c := seg.Ref(ci)
			if c.Kind != facts.RefCall {
				continue
			}
			out = append(out, OutCall{Callee: c.Name, Qualifier: c.Qualifier, QualKind: c.QualKind, Line: c.Line})
		}
		return out
	}
	return nil
}

// Resolve returns the declarations a call made from package fromPkg can
// target, and how they were matched: a package-qualified or unqualified call
// is import_resolved to that package's declarations; a call on a value
// (x.M()) matches methods named M, name_matched when one exists and
// ambiguous otherwise (same-package candidates first, at most 8).
func (v *View) Resolve(fromPkg string, c OutCall) ([]Symbol, string) {
	switch c.QualKind {
	case facts.QualPackage, facts.QualNone:
		pkg := fromPkg
		if c.QualKind == facts.QualPackage {
			pkg = c.Qualifier
		}
		var out []Symbol
		v.eachNamed(c.Callee, func(i, k int) {
			seg := v.sn.Segment(i)
			if kind := seg.SymbolKind(k); kind == facts.KindEmbed || kind == facts.KindMethod {
				return
			}
			if v.meta(i, seg.SymbolFile(k)).Package == pkg {
				out = append(out, v.symbol(i, k))
			}
		})
		if len(out) == 0 {
			return nil, ""
		}
		sortSymbols(out)
		return out, ImportResolved
	default:
		// A method called on a value can only be declared in the caller's
		// package or one it imports (possibly indirectly through a type it
		// gets from an import; those are missed, which keeps noise out).
		var same, other []Symbol
		visible := v.visibleFrom(fromPkg)
		for _, s := range v.Symbols(c.Callee, facts.KindMethod) {
			if s.Name != c.Callee || !visible[s.Package] {
				continue
			}
			if s.Package == fromPkg {
				same = append(same, s)
			} else {
				other = append(other, s)
			}
		}
		out := append(same, other...)
		switch {
		case len(out) == 0:
			return nil, ""
		case len(out) == 1:
			return out, NameMatched
		}
		if len(out) > maxCandidates {
			out = out[:maxCandidates]
		}
		return out, Ambiguous
	}
}

// Incoming is a call site that may target a declaration, with its caller.
type Incoming struct {
	Caller     Symbol
	Line       int
	Resolution string
	Candidates int // declarations the site could equally target (>= 1)
}

// IncomingCalls returns the call sites that can target s, resolved the same
// way as Resolve: package-qualified and same-package calls of functions are
// import_resolved; calls on values of a method named like s are
// name_matched (or ambiguous when several methods share the name).
func (v *View) IncomingCalls(s Symbol) []Incoming {
	var methods []Symbol
	if s.Kind == facts.KindMethod {
		for _, m := range v.Symbols(s.Name, facts.KindMethod) {
			if m.Name == s.Name {
				methods = append(methods, m)
			}
		}
	}
	var out []Incoming
	for _, site := range v.CallSites(s.Name) {
		if site.Caller == nil || site.Caller.ID == s.ID {
			continue
		}
		in := Incoming{Caller: *site.Caller, Line: site.Line, Candidates: 1}
		switch {
		case s.Kind == facts.KindMethod && site.QualKind == facts.QualExpr:
			visible := v.visibleFrom(site.Caller.Package)
			if !visible[s.Package] {
				continue
			}
			n := 0
			for _, m := range methods {
				if visible[m.Package] {
					n++
				}
			}
			in.Resolution, in.Candidates = NameMatched, max(1, n)
			if n > 1 {
				in.Resolution = Ambiguous
			}
		case s.Kind != facts.KindMethod && site.QualKind == facts.QualPackage && site.Qualifier == s.Package,
			s.Kind != facts.KindMethod && site.QualKind == facts.QualNone && site.Caller.Package == s.Package:
			in.Resolution = ImportResolved
		default:
			continue
		}
		out = append(out, in)
	}
	return out
}

// visibleFrom returns pkg and the packages its files import.
func (v *View) visibleFrom(pkg string) map[string]bool {
	if m, ok := v.visible[pkg]; ok {
		return m
	}
	m := map[string]bool{pkg: true}
	for _, imp := range v.PackageImports(pkg) {
		m[imp] = true
	}
	if v.visible == nil {
		v.visible = map[string]map[string]bool{}
	}
	v.visible[pkg] = m
	return m
}

// IsTest reports whether s is a Go test, example, fuzz or benchmark function.
func IsTest(s Symbol) bool {
	if !strings.HasSuffix(s.File, "_test.go") || s.Receiver != "" {
		return false
	}
	for _, p := range []string{"Test", "Example", "Fuzz", "Benchmark"} {
		if strings.HasPrefix(s.Name, p) {
			return true
		}
	}
	return false
}
