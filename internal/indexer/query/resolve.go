package query

import (
	"path"
	"slices"
	"strings"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/segment"
)

// maxCandidates bounds the targets returned for one ambiguous reference.
const maxCandidates = 8

// maxTypeDepth bounds chained type inference (a.b.c, f() returning g()).
const maxTypeDepth = 3

// OutCall is one call or instantiation made inside a declaration.
type OutCall struct {
	Callee, Qualifier string
	QualKind          uint8
	Line              int

	seg, ref int // the reference record, for resolution
}

// Outgoing returns the calls and instantiations made inside declaration s,
// in line order.
func (v *View) Outgoing(s Symbol) []OutCall {
	i, k, ok := v.locate(s)
	if !ok {
		return nil
	}
	seg := v.sn.Segment(i)
	var out []OutCall
	for _, ci := range seg.RefsFrom(k) {
		c := seg.Ref(ci)
		if c.Kind != facts.RefCall && c.Kind != facts.RefInstantiate {
			continue
		}
		out = append(out, OutCall{Callee: c.Name, Qualifier: c.Qualifier, QualKind: c.QualKind, Line: c.Line, seg: i, ref: ci})
	}
	return out
}

// locate finds the segment and symbol index of s.
func (v *View) locate(s Symbol) (int, int, bool) {
	ref, ok := v.sn.Lookup(s.File)
	if !ok {
		return 0, 0, false
	}
	i, f := int(ref.Seg), int(ref.File)
	seg := v.sn.Segment(i)
	for _, k := range seg.SymbolsInFile(f) {
		if seg.SymbolName(k) == s.Name && seg.SymbolKind(k) == s.Kind && seg.Symbol(k).Line == s.Line {
			return i, k, true
		}
	}
	return 0, 0, false
}

// Resolve returns the declarations call c (from Outgoing) can target and
// how they were matched. See resolveRef for the order of the steps.
func (v *View) Resolve(c OutCall) ([]Symbol, string) {
	return v.resolveRef(c.seg, c.ref)
}

// Incoming is a call site that may target a declaration, with its caller.
type Incoming struct {
	Caller     Symbol
	Line       int
	Resolution string
	Candidates int // declarations the site could equally target (>= 1)
}

// IncomingCalls returns the call and instantiation sites that resolve to
// s, with the caller and label of each. A constructor is reached through
// its class's name.
func (v *View) IncomingCalls(s Symbol) []Incoming {
	name := s.Name
	if s.Kind == facts.KindConstructor && s.Receiver != "" {
		name = facts.BaseType(s.Receiver)
	}
	var out []Incoming
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
			targets, label := v.resolveRef(i, r)
			if !targetsInclude(targets, s) {
				continue
			}
			cs := v.symbol(i, caller)
			if cs.ID == s.ID {
				continue
			}
			out = append(out, Incoming{Caller: cs, Line: seg.Ref(r).Line, Resolution: label, Candidates: max(1, len(targets))})
		}
	}
	return out
}

func targetsInclude(targets []Symbol, s Symbol) bool {
	for _, t := range targets {
		if t.ID == s.ID {
			return true
		}
		// new T() resolves to the class; its constructor is the callee.
		if s.Kind == facts.KindConstructor && t.File == s.File && t.Name == facts.BaseType(s.Receiver) && isTypeKind(t.Kind) {
			return true
		}
	}
	return false
}

// Kinds a reference can target.
var (
	callableKinds = map[string]bool{
		facts.KindFunc: true, facts.KindClass: true, facts.KindStruct: true, facts.KindConstructor: true,
		facts.KindMacro: true, facts.KindType: true, facts.KindVar: true, facts.KindConst: true,
		facts.KindInterface: true, facts.KindTypeAlias: true,
	}
	typeKinds = map[string]bool{
		facts.KindClass: true, facts.KindStruct: true, facts.KindInterface: true, facts.KindTrait: true,
		facts.KindProtocol: true, facts.KindEnum: true, facts.KindType: true, facts.KindTypeAlias: true,
	}
)

func isTypeKind(k string) bool { return typeKinds[k] }

// Languages where an unqualified call inside a class may be a method of
// that class (implicit this).
var implicitThis = map[string]bool{
	"java": true, "csharp": true, "kotlin": true, "swift": true, "scala": true, "cpp": true, "dart": true, "ruby": true,
}

// Languages where a unit's declarations are visible to its other files
// without an import.
var unitVisible = map[string]bool{
	"go": true, "java": true, "kotlin": true, "scala": true, "csharp": true, "swift": true, "dart": true,
	"c": true, "cpp": true, "objc": true,
}

// Languages whose module imports (not just listed names) bring names into
// scope unqualified.
var importAll = map[string]bool{"c": true, "cpp": true, "objc": true, "dart": true, "csharp": true, "shell": true, "ruby": true}

// fileCtx is the per-file context resolution needs; imports and hints are
// decoded on first use, once per view.
type fileCtx struct {
	seg, file int
	meta      segment.FileMeta
	sn        *segment.Segment

	importsDone, hintsDone bool
	importList             []facts.Import
	hintList               []segment.HintRec
}

func (fc *fileCtx) imports() []facts.Import {
	if !fc.importsDone {
		fc.importsDone = true
		for _, ii := range fc.sn.ImportsInFile(fc.file) {
			fc.importList = append(fc.importList, fc.sn.Import(ii).Import)
		}
	}
	return fc.importList
}

func (fc *fileCtx) hints() []segment.HintRec {
	if !fc.hintsDone {
		fc.hintsDone = true
		fc.hintList = fc.sn.Hints(fc.file)
	}
	return fc.hintList
}

func (v *View) fileCtx(i, f int) *fileCtx {
	key := fileKey{i, f}
	if fc, ok := v.files[key]; ok {
		return fc
	}
	fc := &fileCtx{seg: i, file: f, meta: v.meta(i, f), sn: v.sn.Segment(i)}
	if v.files == nil {
		v.files = map[fileKey]*fileCtx{}
	}
	v.files[key] = fc
	return fc
}

// resolveRef resolves reference r of segment i. The steps, strongest
// first, each setting the label:
//
//   - f(): the same file, the caller's own class (languages with implicit
//     this), names the file imports, the same unit (languages where units
//     share scope): import_resolved. Then any declaration of that name:
//     name_matched, or ambiguous when several match (not for Go, where an
//     unqualified name is always local or dot-imported).
//   - pkg.f() where pkg is bound by an import: the import's workspace units
//     (import_resolved); an import that maps to no workspace unit is
//     external and resolves to nothing.
//   - x.m(): x's type from self/this, binding hints, fields (a.b) and call
//     results, then methods of that type and the types it extends
//     (type_hinted). Without a type, methods named m: name_matched or
//     ambiguous (Go: only in the caller's package and its imports). A type
//     known to be external resolves to nothing.
func (v *View) resolveRef(i, r int) ([]Symbol, string) {
	key := fileKey{i, r}
	if res, ok := v.resolved[key]; ok {
		return res.targets, res.label
	}
	targets, label := v.resolveRefUncached(i, r)
	if v.resolved == nil {
		v.resolved = map[fileKey]resolution{}
	}
	v.resolved[key] = resolution{targets, label}
	return targets, label
}

type resolution struct {
	targets []Symbol
	label   string
}

func (v *View) resolveRefUncached(i, r int) ([]Symbol, string) {
	seg := v.sn.Segment(i)
	rec := seg.Ref(r)
	fc := v.fileCtx(i, rec.File)
	kinds := callableKinds
	switch rec.Kind {
	case facts.RefInstantiate, facts.RefTypeUse, facts.RefExtends, facts.RefImplements:
		kinds = typeKinds
	}
	caller := rec.Enclosing
	switch rec.QualKind {
	case facts.QualNone:
		return v.resolveUnqualified(fc, caller, rec, kinds)
	case facts.QualPackage:
		return v.resolveImported(fc, rec, kinds)
	default:
		return v.resolveMember(fc, caller, rec, kinds)
	}
}

func (v *View) resolveUnqualified(fc *fileCtx, caller int, rec segment.RefRec, kinds map[string]bool) ([]Symbol, string) {
	name, lang := rec.Name, fc.meta.Lang
	if lang == "go" && goBuiltins[name] && !v.declaredIn(name, fc.meta.Package) {
		return nil, ""
	}
	all := v.named(name, kinds)
	if len(all) == 0 {
		// Implicit this can reach a method even when no free function exists.
		if caller >= 0 && implicitThis[lang] && rec.Kind == facts.RefCall {
			if ms := v.methodsOf(v.receiverOf(fc.seg, caller), name, 0); len(ms) > 0 {
				return capped(ms), ImportResolved
			}
		}
		return nil, ""
	}
	if same := filter(all, func(s Symbol) bool { return s.File == fc.meta.Path }); len(same) > 0 {
		return capped(same), ImportResolved
	}
	if caller >= 0 && implicitThis[lang] && rec.Kind == facts.RefCall {
		if ms := v.methodsOf(v.receiverOf(fc.seg, caller), name, 0); len(ms) > 0 {
			return capped(ms), ImportResolved
		}
	}
	if units := v.importedUnitsFor(fc, name); len(units) > 0 {
		if in := filter(all, func(s Symbol) bool { return units[s.Package] }); len(in) > 0 {
			return capped(in), ImportResolved
		}
	}
	if unitVisible[lang] {
		if in := filter(all, func(s Symbol) bool { return s.Package == fc.meta.Package }); len(in) > 0 {
			return capped(in), ImportResolved
		}
	}
	if lang == "go" {
		return nil, ""
	}
	return v.labelByName(all, fc.meta.Package)
}

// importedUnitsFor returns the units from which the file's imports bring
// name into scope: listed names (from x import name), wildcard imports,
// and whole-module imports in languages where those bring every name.
func (v *View) importedUnitsFor(fc *fileCtx, name string) map[string]bool {
	out := map[string]bool{}
	for _, imp := range fc.imports() {
		brings := imp.Kind == facts.ImportWildcard || imp.Kind == facts.ImportInclude || (importAll[fc.meta.Lang] && len(imp.Names) == 0 && imp.Name == "")
		for _, n := range imp.Names {
			if n.Name == name || n.Alias == name {
				brings = true
			}
		}
		if !brings {
			continue
		}
		for _, u := range v.importUnits(fc, imp.Path) {
			out[u] = true
		}
	}
	return out
}

func (v *View) resolveImported(fc *fileCtx, rec segment.RefRec, kinds map[string]bool) ([]Symbol, string) {
	spec := rec.Qualifier
	units := map[string]bool{}
	for _, u := range v.importUnits(fc, spec) {
		units[u] = true
	}
	if len(units) == 0 {
		return nil, "" // an external package
	}
	// The import may name a type (Java a.b.Helper, Rust a::Helper): its
	// static members come first.
	owner := lastSegment(spec)
	var members, free []Symbol
	v.eachNamed(rec.Name, func(i, k int) {
		s := v.symbol(i, k)
		if !units[s.Package] || s.Kind == facts.KindEmbed {
			return
		}
		switch {
		case s.Receiver != "" && facts.BaseType(s.Receiver) == owner:
			members = append(members, s)
		case s.Kind != facts.KindMethod && (kinds[s.Kind] || fc.meta.Lang == "go"):
			free = append(free, s)
		}
	})
	if len(members) > 0 {
		return capped(members), ImportResolved
	}
	if len(free) > 0 {
		return capped(free), ImportResolved
	}
	return nil, ""
}

func (v *View) resolveMember(fc *fileCtx, caller int, rec segment.RefRec, kinds map[string]bool) ([]Symbol, string) {
	typ, known := v.qualifierType(fc, caller, rec.Qualifier, 0)
	if typ != "" {
		if ms := v.methodsOf(typ, rec.Name, 0); len(ms) > 0 {
			ms = v.preferVisible(fc, ms)
			if len(ms) == 1 {
				return ms, TypeHinted
			}
			return capped(ms), Ambiguous
		}
		if rec.Kind == facts.RefInstantiate { // new ns.T() with ns a namespace value
			if ts := v.named(rec.Name, kinds); len(ts) > 0 {
				return v.labelByName(ts, fc.meta.Package)
			}
		}
	}
	if known {
		return nil, "" // the receiver's type is outside the workspace
	}
	// No type: methods by name.
	var ms []Symbol
	if fc.meta.Lang == "go" {
		visible := v.visibleFrom(fc.meta.Package)
		ms = filter(v.named(rec.Name, map[string]bool{facts.KindMethod: true}), func(s Symbol) bool { return visible[s.Package] })
	} else {
		ms = v.named(rec.Name, map[string]bool{facts.KindMethod: true, facts.KindFunc: true, facts.KindConstructor: true})
		if rec.Kind == facts.RefInstantiate {
			ms = v.named(rec.Name, kinds)
		}
	}
	if len(ms) == 0 {
		return nil, ""
	}
	return v.labelByName(ms, fc.meta.Package)
}

// byName labels a name-only match: name_matched for one candidate,
// ambiguous otherwise (same-unit candidates first).
func (v *View) labelByName(cands []Symbol, unit string) ([]Symbol, string) {
	if len(cands) == 1 {
		return cands, NameMatched
	}
	slices.SortStableFunc(cands, func(a, b Symbol) int {
		ai, bi := a.Package == unit, b.Package == unit
		switch {
		case ai && !bi:
			return -1
		case bi && !ai:
			return 1
		}
		return 0
	})
	return capped(cands), Ambiguous
}

// preferVisible keeps candidates in the file's unit or its imported units
// when that leaves any.
func (v *View) preferVisible(fc *fileCtx, cands []Symbol) []Symbol {
	if len(cands) <= 1 {
		return cands
	}
	units := map[string]bool{fc.meta.Package: true}
	for _, imp := range fc.imports() {
		for _, u := range v.importUnits(fc, imp.Path) {
			units[u] = true
		}
	}
	if in := filter(cands, func(s Symbol) bool { return units[s.Package] }); len(in) > 0 {
		return in
	}
	return cands
}

// named returns the live declarations called name of the given kinds, in
// symbol order. Each name is decoded once per view.
func (v *View) named(name string, kinds map[string]bool) []Symbol {
	all, ok := v.byName[name]
	if !ok {
		v.eachNamed(name, func(i, k int) {
			if v.sn.Segment(i).SymbolKind(k) != facts.KindEmbed {
				all = append(all, v.symbol(i, k))
			}
		})
		sortSymbols(all)
		if v.byName == nil {
			v.byName = map[string][]Symbol{}
		}
		v.byName[name] = all
	}
	return filter(all, func(s Symbol) bool { return kinds[s.Kind] })
}

func filter(in []Symbol, keep func(Symbol) bool) []Symbol {
	var out []Symbol
	for _, s := range in {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

func capped(s []Symbol) []Symbol {
	if len(s) > maxCandidates {
		return s[:maxCandidates]
	}
	return s
}

// receiverOf returns the owning type of symbol k (its receiver, or the
// receiver of the function it is nested in).
func (v *View) receiverOf(i, k int) string {
	seg := v.sn.Segment(i)
	for depth := 0; k >= 0 && depth < 8; depth++ {
		if r := seg.SymbolReceiver(k); r != "" {
			return facts.BaseType(r)
		}
		if isTypeKind(seg.SymbolKind(k)) {
			return seg.SymbolName(k)
		}
		k = seg.SymbolParent(k)
	}
	return ""
}

// methodsOf returns the members named name of type typ (base name, or
// qualified as written), following extends and implements up to
// maxTypeDepth levels when typ declares none.
func (v *View) methodsOf(typ, name string, depth int) []Symbol {
	base := typeBase(typ)
	if base == "" {
		return nil
	}
	var out []Symbol
	v.eachNamed(name, func(i, k int) {
		seg := v.sn.Segment(i)
		if r := seg.SymbolReceiver(k); r != "" && facts.BaseType(r) == base && seg.SymbolKind(k) != facts.KindEmbed {
			out = append(out, v.symbol(i, k))
		}
	})
	if len(out) > 0 || depth >= maxTypeDepth {
		sortSymbols(out)
		return out
	}
	for _, parent := range v.supertypes(base) {
		if ms := v.methodsOf(parent, name, depth+1); len(ms) > 0 {
			out = append(out, ms...)
		}
	}
	sortSymbols(out)
	return out
}

// supertypes returns the names a type extends, implements or embeds.
func (v *View) supertypes(base string) []string {
	var out []string
	v.eachNamed(base, func(i, k int) {
		seg := v.sn.Segment(i)
		if !isTypeKind(seg.SymbolKind(k)) {
			return
		}
		for _, ri := range seg.RefsFrom(k) {
			if kind := seg.RefKind(ri); kind == facts.RefExtends || kind == facts.RefImplements {
				if n := seg.Ref(ri).Name; n != base && !slices.Contains(out, n) {
					out = append(out, n)
				}
			}
		}
		// Go embeds are symbols of the struct or interface.
		f := seg.SymbolFile(k)
		for _, e := range seg.SymbolsInFile(f) {
			if seg.SymbolParent(e) == k && seg.SymbolKind(e) == facts.KindEmbed {
				if n := strings.Clone(seg.SymbolName(e)); !slices.Contains(out, n) {
					out = append(out, n)
				}
			}
		}
	})
	return out
}

// qualifierType infers the type of a receiver expression x in x.m(). known
// reports that the expression has a type even if it is not in the
// workspace (a local of type bytes.Buffer): then no name matching should
// be attempted.
func (v *View) qualifierType(fc *fileCtx, caller int, q string, depth int) (typ string, known bool) {
	if q == "" || depth > maxTypeDepth {
		return "", false
	}
	segs := splitQualifier(q)
	first := segs[0]
	switch {
	case isSelf(first):
		if caller < 0 {
			return "", false
		}
		typ = v.receiverOf(fc.seg, caller)
	case strings.HasPrefix(first, "@") && len(first) > 1: // Ruby instance variable
		if caller < 0 {
			return "", false
		}
		typ = v.fieldType(v.receiverOf(fc.seg, caller), first, depth)
	default:
		typ = v.hintType(fc, caller, first, depth)
		if typ == "" && v.externalBinding(fc, first) {
			return "", true // a module or type from outside the workspace
		}
		if typ == "" && startsUpper(first) && len(v.named(first, typeKinds)) > 0 {
			typ = first // a static call on a type: Helper.check()
		}
		if typ == "" && strings.HasSuffix(first, ")") {
			typ = v.callType(strings.TrimSuffix(strings.TrimSuffix(first, ")"), "("), depth)
		}
	}
	if typ == "" {
		return "", false
	}
	for _, field := range segs[1:] {
		next := v.fieldType(typ, field, depth)
		if next == "" {
			return "", false
		}
		typ = next
	}
	return typ, true
}

// externalBinding reports that name is bound by an import that maps to no
// workspace unit (import os; os.path.join()).
func (v *View) externalBinding(fc *fileCtx, name string) bool {
	for _, imp := range fc.imports() {
		bound := imp.Name == name
		for _, n := range imp.Names {
			bound = bound || n.Alias == name || (n.Alias == "" && n.Name == name)
		}
		if !bound && imp.Name == "" && len(imp.Names) == 0 {
			first, _, _ := strings.Cut(strings.TrimLeft(imp.Path, "."), ".")
			bound = first == name || lastSegment(imp.Path) == name
		}
		if bound {
			return len(v.importUnits(fc, imp.Path)) == 0
		}
	}
	return false
}

// hintType returns the type a binding hint gives name in the caller's
// scope: its own hints, then its class's field hints, then file-level ones.
func (v *View) hintType(fc *fileCtx, caller int, name string, depth int) string {
	norm := normName(name)
	class := -1
	if caller >= 0 {
		for _, h := range fc.hints() {
			if h.Scope == caller && (h.Name == name || normName(h.Name) == norm) {
				return v.evalType(fc, h.Type, depth)
			}
		}
		class = v.sn.Segment(fc.seg).SymbolParent(caller)
	}
	if class >= 0 && isTypeKind(v.sn.Segment(fc.seg).SymbolKind(class)) {
		for _, h := range fc.hints() {
			if h.Scope == class && normName(h.Name) == norm {
				return v.evalType(fc, h.Type, depth)
			}
		}
	}
	for _, h := range fc.hints() {
		if h.Scope < 0 && h.Name == name {
			return v.evalType(fc, h.Type, depth)
		}
	}
	return ""
}

// fieldType returns the type of member field of type typ: hints scoped to
// a declaration of the type or to one of its members (a constructor
// parameter stored as a field).
func (v *View) fieldType(typ, field string, depth int) string {
	base := typeBase(typ)
	if base == "" || depth > maxTypeDepth {
		return ""
	}
	norm := normName(field)
	found := ""
	v.eachNamed(base, func(i, k int) {
		seg := v.sn.Segment(i)
		if found != "" || !isTypeKind(seg.SymbolKind(k)) {
			return
		}
		fc := v.fileCtx(i, seg.SymbolFile(k))
		for _, h := range fc.hints() {
			if h.Scope < 0 || normName(h.Name) != norm {
				continue
			}
			if h.Scope == k || seg.SymbolParent(h.Scope) == k {
				found = v.evalType(fc, h.Type, depth+1)
				return
			}
		}
	})
	return found
}

// evalType turns a hint's type into a type name: "f()" and "pkg.f()" are
// the result type of f; "T::new()" and "T.new()" construct T.
func (v *View) evalType(fc *fileCtx, t string, depth int) string {
	if !strings.HasSuffix(t, "()") {
		return t
	}
	return v.callType(strings.TrimSuffix(t, "()"), depth+1)
}

// callType returns the type a call to callee (f, pkg.f, T::new) returns.
func (v *View) callType(callee string, depth int) string {
	if depth > maxTypeDepth {
		return ""
	}
	name, qual := splitLast(callee)
	if name == "new" && qual != "" {
		return qual
	}
	for _, s := range v.named(name, map[string]bool{facts.KindFunc: true, facts.KindMethod: true, facts.KindClass: true, facts.KindStruct: true, facts.KindConstructor: true}) {
		switch {
		case isTypeKind(s.Kind):
			return s.Name // a constructor call in call syntax: Foo()
		case s.Kind == facts.KindConstructor:
			return facts.BaseType(s.Receiver)
		}
		// pkg.f() names a package function; T.f() a member of T.
		if qual != "" && s.Receiver != "" && facts.BaseType(s.Receiver) != typeBase(qual) {
			continue
		}
		if rt := resultType(s.Signature, s.Name); rt != "" {
			if rt == "Self" || rt == "self" || rt == "this" || rt == "instancetype" {
				return facts.BaseType(s.Receiver)
			}
			return rt
		}
	}
	return ""
}

// resultType extracts the declared result type from a signature: Go's
// results after the parameters, "-> T" (Python, Rust, Swift), "): T"
// (TypeScript, Kotlin, Scala, PHP), or the word before the name (Java, C#,
// C, C++, Dart).
func resultType(sig, name string) string {
	at := strings.Index(sig, name+"(")
	if at < 0 {
		at = strings.Index(sig, name+"<")
	}
	if at < 0 {
		return ""
	}
	open := strings.IndexByte(sig[at:], '(')
	if open < 0 {
		return ""
	}
	depth, close := 0, -1
	for j := at + open; j < len(sig); j++ {
		switch sig[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				close = j
			}
		}
		if close >= 0 {
			break
		}
	}
	if close < 0 {
		return ""
	}
	rest := strings.TrimSpace(sig[close+1:])
	switch {
	case strings.HasPrefix(rest, "->"):
		return plainType(strings.TrimSpace(rest[2:]))
	case strings.HasPrefix(rest, ":"):
		return plainType(strings.TrimSpace(rest[1:]))
	case strings.HasPrefix(sig, "func ") || strings.HasPrefix(sig, "func("):
		if strings.HasPrefix(rest, "(") {
			rest = rest[1:]
			if j := strings.IndexAny(rest, ",)"); j >= 0 {
				rest = rest[:j]
			}
		}
		return plainType(rest)
	}
	before := strings.Fields(sig[:at])
	if len(before) == 0 {
		return ""
	}
	return plainType(before[len(before)-1])
}

// plainType strips pointer, reference, nullable, array and generic syntax
// from a type as written; "" when nothing named remains.
func plainType(t string) string {
	t = strings.TrimSpace(t)
	for len(t) > 0 && strings.ContainsRune("*&?^", rune(t[0])) {
		t = t[1:]
	}
	if i := strings.IndexAny(t, "<[({ "); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimRight(t, "?*&!,;:")
	switch t {
	case "", "void", "Unit", "None", "error", "bool", "int", "string", "any":
		return ""
	}
	return t
}

// splitQualifier splits a receiver expression into segments: a.b, a->b,
// a::b and self.x, keeping call parentheses on their segment.
func splitQualifier(q string) []string {
	var out []string
	depth, start := 0, 0
	for j := 0; j < len(q); j++ {
		switch c := q[j]; {
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case depth == 0 && c == '.':
			out = append(out, q[start:j])
			start = j + 1
		case depth == 0 && c == '-' && j+1 < len(q) && q[j+1] == '>':
			out = append(out, q[start:j])
			start = j + 2
			j++
		case depth == 0 && c == ':' && j+1 < len(q) && q[j+1] == ':':
			out = append(out, q[start:j])
			start = j + 2
			j++
		}
	}
	return append(out, q[start:])
}

func splitLast(s string) (name, qual string) {
	best, width := -1, 0
	for _, sep := range []string{"::", ".", "->"} {
		if i := strings.LastIndex(s, sep); i > best {
			best, width = i, len(sep)
		}
	}
	if best < 0 {
		return s, ""
	}
	return s[best+width:], s[:best]
}

func isSelf(s string) bool {
	switch s {
	case "self", "this", "$this", "Self", "super", "base":
		return true
	}
	return false
}

// normName reduces a variable or field name to compare hints across
// spellings: $x, @x, self.x and this.x all become x.
func normName(s string) string {
	for _, p := range []string{"self.", "this.", "$this->", "@"} {
		s = strings.TrimPrefix(s, p)
	}
	return strings.TrimPrefix(s, "$")
}

// typeBase is the last segment of a possibly qualified type name.
func typeBase(t string) string {
	name, _ := splitLast(t)
	return facts.BaseType(name)
}

func lastSegment(spec string) string {
	spec = strings.TrimSuffix(spec, "/")
	if i := strings.LastIndexAny(spec, "./\\:"); i >= 0 {
		return spec[i+1:]
	}
	return spec
}

func startsUpper(s string) bool { return s != "" && s[0] >= 'A' && s[0] <= 'Z' }

// importUnits maps an import spec of file fc to the workspace units it
// names; none means the import is external. Go specs are import paths;
// relative specs resolve against the file's directory; dotted and
// path-like specs match directories and files by suffix (src layouts,
// Java package directories), with the spec's parent tried for imports
// that name a class or module file.
func (v *View) importUnits(fc *fileCtx, spec string) []string {
	lang := fc.meta.Lang
	dir := path.Dir(fc.meta.Path)
	key := lang + "\x00" + spec
	relative := strings.HasPrefix(spec, ".") || lang == "ruby" || lang == "shell" || lang == "c" || lang == "cpp" || lang == "objc" || lang == "dart"
	if relative {
		key += "\x00" + dir
	}
	return v.cache.memoUnits(v.sn.Generation(), key, func() []string {
		return v.computeUnits(lang, dir, spec)
	})
}

func (v *View) computeUnits(lang, dir, spec string) []string {
	if lang == "go" {
		if len(v.packagePaths(spec)) > 0 {
			return []string{spec}
		}
		return nil
	}
	var cands []string
	switch lang {
	case "javascript", "typescript":
		if !strings.HasPrefix(spec, ".") {
			return nil // a package; tsconfig paths and workspaces come with M7
		}
		base := path.Join(dir, spec)
		return v.fileUnits([]string{stripExt(base), path.Join(base, "index")}, true)
	case "python":
		rel := spec
		up := 0
		for strings.HasPrefix(rel, ".") {
			rel, up = rel[1:], up+1
		}
		p := strings.ReplaceAll(rel, ".", "/")
		if up > 0 {
			d := dir
			for n := 1; n < up; n++ {
				d = path.Dir(d)
			}
			full := path.Join(d, p)
			return uniq(append(v.fileUnits([]string{full, path.Join(full, "__init__")}, true), v.dirUnits(full, true)...))
		}
		cands = []string{p}
	case "rust":
		segs := strings.Split(spec, "::")
		switch segs[0] {
		case "crate":
			segs = segs[1:]
		case "self":
			segs = append(strings.Split(dir, "/"), segs[1:]...)
		case "super":
			segs = append(strings.Split(path.Dir(dir), "/"), segs[1:]...)
		}
		cands = []string{strings.Join(segs, "/")}
	case "ruby", "shell", "c", "cpp", "objc":
		rel := strings.TrimPrefix(spec, "./")
		local := path.Join(dir, rel)
		if u := v.fileUnits([]string{stripExt(local)}, true); len(u) > 0 {
			return u
		}
		return v.fileUnits([]string{stripExt(rel)}, false)
	case "dart":
		rel := spec
		if strings.HasPrefix(rel, "package:") {
			rel = rel[len("package:"):]
			if i := strings.IndexByte(rel, '/'); i >= 0 {
				rel = "lib/" + rel[i+1:]
			}
		} else if strings.HasPrefix(rel, "dart:") {
			return nil
		} else {
			rel = path.Join(dir, rel)
		}
		if u := v.fileUnits([]string{stripExt(rel)}, true); len(u) > 0 {
			return u
		}
		return v.fileUnits([]string{stripExt(rel)}, false)
	case "php":
		cands = []string{strings.ReplaceAll(strings.Trim(spec, "\\"), "\\", "/")}
	default: // java, kotlin, scala, csharp, swift and others: dotted
		cands = []string{strings.ReplaceAll(spec, ".", "/")}
	}
	var out []string
	for _, c := range cands {
		for _, p := range []string{c, path.Dir(c)} {
			if p == "." || p == "" {
				continue
			}
			out = append(out, v.dirUnits(p, false)...)
			out = append(out, v.fileUnits([]string{p}, false)...)
		}
	}
	if len(out) == 0 && (lang == "csharp" || lang == "php") {
		// Namespaces need not mirror directories: try the spec without its
		// leading segments (Company.Product.Store -> Product/Store, Store).
		c := cands[0]
		for i := strings.IndexByte(c, '/'); i >= 0 && len(out) == 0; i = strings.IndexByte(c, '/') {
			c = c[i+1:]
			out = append(out, v.dirUnits(c, false)...)
		}
	}
	return uniq(out)
}

// dirUnits returns the units (directories) equal to p, or ending in /p
// unless exact.
func (v *View) dirUnits(p string, exact bool) []string {
	var out []string
	for _, pkg := range v.Packages() {
		if pkg == p || (!exact && strings.HasSuffix(pkg, "/"+p)) {
			out = append(out, pkg)
		}
	}
	return out
}

// fileUnits returns the units of the live files whose path without
// extension is one of stems, or ends in /stem unless exact.
func (v *View) fileUnits(stems []string, exact bool) []string {
	var out []string
	for _, stem := range stems {
		base := stem[strings.LastIndexByte(stem, '/')+1:]
		for i := 0; i < v.sn.NumSegments(); i++ {
			seg := v.sn.Segment(i)
			for _, f := range v.cache.fileStems(v.sn, seg)[base] {
				if !v.sn.Live(i, int(f)) {
					continue
				}
				p := stripExt(seg.FilePathView(int(f)))
				if p == stem || (!exact && strings.HasSuffix(p, "/"+stem)) {
					out = append(out, seg.FilePackage(int(f)))
				}
			}
		}
	}
	return uniq(out)
}

func stripExt(p string) string {
	base := p[strings.LastIndexByte(p, '/')+1:]
	if dot := strings.IndexByte(base, '.'); dot > 0 {
		return p[:len(p)-len(base)+dot]
	}
	return p
}

func uniq(in []string) []string {
	slices.Sort(in)
	return slices.Compact(in)
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
