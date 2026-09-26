package query

import (
	"path"
	"reflect"
	"slices"
	"strings"

	"github.com/spawn08/chronos-code/indexer/facts"
	"github.com/spawn08/chronos-code/indexer/segment"
)

// maxCandidates bounds the targets returned for one ambiguous reference.
const maxCandidates = 8

// maxTypeDepth bounds chained type inference (a.b.c, f() returning g()).
const maxTypeDepth = 3

// OutCall is one call or instantiation made inside a declaration.
type OutCall struct {
	Callee, Qualifier string
	QualKind          uint8
	Kind              uint8 // facts.RefCall, facts.RefInstantiate or facts.RefContract
	Line              int

	seg, ref int // the reference record, for resolution
}

// Outgoing returns the calls and instantiations made inside declaration s,
// and the contracts it uses (facts.RefContract: a client call of a route
// or RPC, a message produced to a topic), in line order.
func (v *View) Outgoing(s Symbol) []OutCall {
	i, k, ok := v.locate(s)
	if !ok {
		return nil
	}
	seg := v.sn.Segment(i)
	var out []OutCall
	for _, ci := range seg.RefsFrom(k) {
		c := seg.Ref(ci)
		if c.Kind != facts.RefCall && c.Kind != facts.RefInstantiate && c.Kind != facts.RefContract {
			continue
		}
		out = append(out, OutCall{Callee: c.Name, Qualifier: c.Qualifier, QualKind: c.QualKind, Kind: c.Kind, Line: c.Line, seg: i, ref: ci})
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
// its class's name. For a contract node they are its uses (RefContract).
func (v *View) IncomingCalls(s Symbol) []Incoming {
	names := []string{s.Name}
	switch {
	case s.Kind == facts.KindConstructor && s.Receiver != "":
		names[0] = facts.BaseType(s.Receiver)
	case s.Kind == facts.KindRoute:
		names = routeKeys(s.Name)
	}
	var out []Incoming
	var ctors *ctorSet
	if s.Kind == facts.KindConstructor {
		ctors = v.constructorsOf(s)
	}
	contract := facts.ContractKinds[s.Kind]
	for i := 0; i < v.sn.NumSegments(); i++ {
		seg := v.sn.Segment(i)
		for _, name := range names {
			out = v.incomingNamed(out, s, i, seg, name, contract, ctors)
		}
	}
	return out
}

func (v *View) incomingNamed(out []Incoming, s Symbol, i int, seg *segment.Segment, name string, contract bool, ctors *ctorSet) []Incoming {
	lo, hi := seg.RefsTo(name)
	for r := lo; r < hi; r++ {
		kind := seg.RefKind(r)
		if contract != (kind == facts.RefContract) || (!contract && kind != facts.RefCall && kind != facts.RefInstantiate) || !v.sn.Live(i, seg.RefFile(r)) {
			continue
		}
		caller := seg.RefEnclosing(r)
		if caller < 0 {
			continue
		}
		targets, label := v.resolveRef(i, r)
		if !targetsInclude(targets, s) || !ctors.selects(s, seg.Ref(r)) {
			continue
		}
		cs := v.symbol(i, caller)
		if cs.ID == s.ID {
			continue
		}
		out = append(out, Incoming{Caller: cs, Line: seg.Ref(r).Line, Resolution: label, Candidates: max(1, len(targets))})
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

// ctorSet holds the constructors of one class, to tell which of them a
// call with a given argument count runs (new T(a, b)).
type ctorSet struct{ all []Symbol }

// constructorsOf returns the constructors of c's class (same unit and
// receiver), c included.
func (v *View) constructorsOf(c Symbol) *ctorSet {
	set := &ctorSet{}
	v.eachNamed(c.Name, func(i, k int) {
		s := v.symbol(i, k)
		if s.Kind == facts.KindConstructor && s.Package == c.Package && facts.BaseType(s.Receiver) == facts.BaseType(c.Receiver) {
			set.all = append(set.all, s)
		}
	})
	return set
}

// selects reports whether constructor c can be the one call site rec runs:
// in languages with overloading, when the argument count is known and some
// constructor of the class accepts it, c must accept it too. A definition
// also accepts what a bodiless declaration with as many parameters accepts
// (C++ defaults on the declaration). A nil set selects every constructor.
func (cs *ctorSet) selects(c Symbol, rec segment.RefRec) bool {
	n, ok := rec.NArgs()
	if cs == nil || !ok || !overloading[c.Lang] {
		return true
	}
	accepts := func(s Symbol) bool {
		if s.Params.Accepts(n) {
			return true
		}
		for _, d := range cs.all {
			if d.Decl && d.Params.Known && d.Params.Max == s.Params.Max && d.Params.Accepts(n) {
				return true
			}
		}
		return false
	}
	if accepts(c) {
		return true
	}
	return !slices.ContainsFunc(cs.all, accepts)
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

// Languages whose lambdas can have an implicit receiver, so an unqualified
// call may be a method of any type.
var implicitReceiver = map[string]bool{"kotlin": true, "swift": true, "scala": true, "ruby": true}

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
	targets = activeFirst(definitionsFirst(targets))
	if len(targets) > 1 {
		targets = callerLast(targets, v.sn.Segment(i).RefEnclosing(r), func(k int) Symbol { return v.symbol(i, k) })
	}
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
	switch rec.Kind {
	case facts.RefContract:
		return v.resolveContract(rec.Qualifier, rec.Name)
	case facts.RefMention:
		return v.resolveMention(fc, rec)
	}
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
	fit := func(c []Symbol) []Symbol { return v.fitCall(fc, caller, rec, c, 0) }
	if lang != "go" {
		if as := v.importedAs(fc, name, kinds); len(as) > 0 {
			return capped(fit(as)), ImportResolved
		}
	}
	if rec.Kind == facts.RefCall && rec.Lambda >= 0 {
		// Inside a lambda with a receiver (Kotlin inOrder(a) { verify(a) }):
		// the receiver's members come before top-level functions.
		if ms := v.lambdaReceiverMembers(fc.seg, rec.Lambda, name); len(ms) > 0 {
			return capped(fit(ms)), TypeHinted
		}
	}
	all := v.named(name, kinds)
	if len(all) == 0 {
		// Implicit this and static imports can reach a method even when no
		// free function exists.
		if caller >= 0 && implicitThis[lang] && rec.Kind == facts.RefCall {
			if ms := v.implicitThis(fc, caller, name); len(ms) > 0 {
				return capped(fit(ms)), ImportResolved
			}
		}
		if rec.Kind == facts.RefCall {
			if ms := v.importedMembers(fc, name); len(ms) > 0 {
				return capped(fit(ms)), ImportResolved
			}
			// A lambda with a receiver (Kotlin apply { f() }, Ruby
			// instance_eval) calls methods of a receiver we cannot see.
			if implicitReceiver[lang] {
				if ms := v.named(name, map[string]bool{facts.KindMethod: true}); len(ms) > 0 {
					return v.labelByName(v.inBuildDeps(fc, fit(v.preferVisible(fc, ms))), fc.meta.Package)
				}
			}
		}
		return nil, ""
	}
	if same := filter(all, func(s Symbol) bool { return s.File == fc.meta.Path }); len(same) > 0 {
		same = nearestFirst(same, rec.Line)
		if packageScoped[lang] && rec.Kind == facts.RefCall {
			// Top-level functions of one package overload each other
			// across files (Kotlin, Scala): the same-file ones come first.
			same = append(slices.Clone(same), filter(all, func(s Symbol) bool {
				return s.File != fc.meta.Path && s.Kind == facts.KindFunc && v.sameUnit(fc, s)
			})...)
		}
		return capped(fit(same)), ImportResolved
	}
	if caller >= 0 && implicitThis[lang] && rec.Kind == facts.RefCall {
		if ms := v.implicitThis(fc, caller, name); len(ms) > 0 {
			return capped(fit(ms)), ImportResolved
		}
	}
	// Names an import lists explicitly shadow the unit's own declarations,
	// which shadow names reached by wildcard or whole-module imports (Java
	// single-type imports, then the package, then on-demand imports; C#
	// the enclosing namespace before using directives).
	sc := v.importedUnitsFor(fc, name)
	if in := filter(all, sc.hasNamed); len(in) > 0 {
		return capped(fit(in)), ImportResolved
	}
	if sc.anyNamed() {
		// Re-exports: barrels (export … from), Python package __init__
		// imports, Rust pub use.
		if in := v.declaredAt(v.reexported(fc, name), kinds); len(in) > 0 {
			return capped(fit(in)), ImportResolved
		}
	}
	if rec.Kind == facts.RefCall && sc.anyNamed() {
		if ms := v.importedMembers(fc, name); len(ms) > 0 {
			return capped(fit(ms)), ImportResolved
		}
	}
	if unitVisible[lang] {
		if in := filter(all, func(s Symbol) bool { return v.sameUnit(fc, s) }); len(in) > 0 {
			return capped(fit(in)), ImportResolved
		}
	}
	if in := filter(all, sc.hasBroad); len(in) > 0 {
		return capped(fit(in)), ImportResolved
	}
	if lang == "go" {
		return nil, ""
	}
	return v.labelByName(v.inBuildDeps(fc, fit(all)), fc.meta.Package)
}

// sameUnit reports whether s is in the file's unit: its directory, the
// same declared package or namespace (Java, Kotlin, Scala, C#; C# code
// also sees its enclosing namespaces), or the same SwiftPM target.
//
// In package-scoped languages only top-level declarations are visible by
// simple name across files: a nested class or a member needs its owner.
func (v *View) sameUnit(fc *fileCtx, s Symbol) bool {
	lang := fc.meta.Lang
	if packageScoped[lang] && s.ParentType {
		return false
	}
	if s.Package == fc.meta.Package {
		return true
	}
	switch {
	case s.Lang != lang:
		return false
	case packageScoped[lang] && fc.meta.PkgName != "" && s.PkgName != "":
		return s.PkgName == fc.meta.PkgName || lang == "csharp" && strings.HasPrefix(fc.meta.PkgName, s.PkgName+".")
	case lang == "swift":
		return v.sameTarget(path.Dir(s.File), path.Dir(fc.meta.Path))
	}
	return false
}

// byArity keeps the candidates of a call or instantiation that accept its
// argument count, in languages with overloading, when the count is known
// and some candidate accepts it; otherwise cands is returned unchanged.
// Candidates of unknown arity (fields, classes, unread parameter lists)
// always stay. extra is added to the count: 1 for a C# extension method,
// whose this parameter is the receiver.
//
// C++ default arguments are written on the declaration, not on the
// out-of-line definition, so a definition also accepts what a bodiless
// declaration of the same member with as many parameters accepts. Apply
// byArity before definitionsFirst, which drops those declarations.
func byArity(lang string, rec segment.RefRec, cands []Symbol, extra int) []Symbol {
	n, ok := rec.NArgs()
	if !ok || len(cands) < 2 || !overloading[lang] {
		return cands
	}
	n += extra
	type member struct {
		key string
		max int
	}
	var declared map[member]bool // declarations accepting n, by member and parameter count
	for _, s := range cands {
		if s.Decl && s.Params.Known && s.Params.Accepts(n) {
			if declared == nil {
				declared = map[member]bool{}
			}
			declared[member{s.Kind + "\x00" + s.Qualified(), s.Params.Max}] = true
		}
	}
	fit := filter(cands, func(s Symbol) bool {
		return s.Params.Accepts(n) || declared[member{s.Kind + "\x00" + s.Qualified(), s.Params.Max}]
	})
	if len(fit) == 0 {
		return cands
	}
	return fit
}

// activeFirst drops C-family declarations in branches the default
// configuration does not compile when a compiled one is also a candidate
// (a typedef made once per #if branch).
func activeFirst(targets []Symbol) []Symbol {
	if len(targets) < 2 || !slices.ContainsFunc(targets, func(t Symbol) bool { return t.Inactive }) {
		return targets
	}
	if in := filter(targets, func(t Symbol) bool { return !t.Inactive }); len(in) > 0 {
		return in
	}
	return targets
}

// implicitThis returns the methods named name of the caller's class, for
// an unqualified call (implicit this). In languages with overloading the
// supertypes' overloads with other parameter lists are candidates too.
func (v *View) implicitThis(fc *fileCtx, caller int, name string) []Symbol {
	recv := v.receiverOf(fc.seg, caller)
	ms := preferCallable(v.methodsOf(recv, name, 0))
	if len(ms) > 0 && overloading[fc.meta.Lang] {
		ms = v.inheritedCallables(fc.meta.Lang, recv, name, ms)
	}
	return ms
}

// callerLast moves the calling declaration behind the other candidates: a
// call to its own name inside one overload usually delegates to another
// overload rather than recursing.
func callerLast(targets []Symbol, caller int, symbol func(int) Symbol) []Symbol {
	if caller < 0 {
		return targets
	}
	c := symbol(caller)
	at := slices.IndexFunc(targets, func(t Symbol) bool { return t.ID == c.ID })
	if at < 0 || at == len(targets)-1 {
		return targets
	}
	out := make([]Symbol, 0, len(targets))
	out = append(out, targets[:at]...)
	out = append(out, targets[at+1:]...)
	return append(out, targets[at])
}

// definitionsFirst drops bodiless declarations (a C++ member declared in
// its class and defined out of line, a C prototype) when a definition of
// the same member is also a candidate: the definition is the target.
func definitionsFirst(targets []Symbol) []Symbol {
	if len(targets) < 2 || !slices.ContainsFunc(targets, func(t Symbol) bool { return t.Decl }) {
		return targets
	}
	defined := map[string]bool{}
	for _, t := range targets {
		if !t.Decl {
			defined[t.Kind+"\x00"+t.Qualified()] = true
		}
	}
	if len(defined) == 0 {
		return targets
	}
	return filter(targets, func(t Symbol) bool { return !t.Decl || !defined[t.Kind+"\x00"+t.Qualified()] })
}

// importedMembers returns the methods an import of one member brings into
// scope unqualified (Java import static a.b.C.m): members named name of the
// import's owner type, in the import's workspace units.
func (v *View) importedMembers(fc *fileCtx, name string) []Symbol {
	if fc.meta.Lang == "go" {
		return nil
	}
	var out []Symbol
	for _, imp := range fc.imports() {
		if len(imp.Names) > 0 || imp.Kind == facts.ImportWildcard || lastSegment(imp.Path) != name {
			continue
		}
		_, owner := splitLast(imp.Path)
		if owner == "" {
			continue
		}
		units := map[string]bool{}
		for _, u := range v.importUnits(fc, imp.Path) {
			units[u] = true
		}
		out = append(out, filter(v.methodsOf(owner, name, 0), func(s Symbol) bool { return units[s.Package] })...)
	}
	return out
}

// nearestFirst orders same-file definitions of one name for a reference
// on line: the closest definition before it first (a nested helper or a
// class redefined per test), then those after it.
func nearestFirst(cands []Symbol, line int) []Symbol {
	if len(cands) < 2 {
		return cands
	}
	out := slices.Clone(cands)
	slices.SortStableFunc(out, func(a, b Symbol) int {
		ab, bb := a.Line <= line, b.Line <= line
		switch {
		case ab && !bb:
			return -1
		case bb && !ab:
			return 1
		case ab: // both before: later is nearer
			return b.Line - a.Line
		}
		return a.Line - b.Line
	})
	return out
}

// importedUnitsFor returns the units from which the file's imports bring
// name into scope: named are imports that name it (from x import name,
// Java import a.b.C and import static a.b.C.m, Rust use a::b::f); broad
// are wildcard imports and whole-module imports in languages where those
// bring every name.
//
// In package-scoped languages the imports also name declared packages:
// import a.b.C names C of package a.b, import a.b.* and C#'s using a.b
// bring package a.b, wherever its files are.
func (v *View) importedUnitsFor(fc *fileCtx, name string) importScope {
	lang := fc.meta.Lang
	sc := importScope{lang: lang, named: map[string]bool{}, broad: map[string]bool{}}
	for _, imp := range fc.imports() {
		into, intoPkg := sc.broad, &sc.broadPkgs
		brings := imp.Kind == facts.ImportWildcard || imp.Kind == facts.ImportInclude || (importAll[lang] && len(imp.Names) == 0 && imp.Name == "")
		pkg := imp.Path // the package a wildcard or using directive brings
		if lang != "go" && imp.Kind != facts.ImportWildcard && len(imp.Names) == 0 && (imp.Name == name || imp.Name == "" && lastSegment(imp.Path) == name) {
			brings, into, intoPkg = true, sc.named, &sc.namedPkgs
			_, pkg = splitLast(imp.Path)
		}
		for _, n := range imp.Names {
			if n.Name == name || n.Alias == name {
				brings, into, intoPkg = true, sc.named, &sc.namedPkgs
			}
		}
		if !brings {
			continue
		}
		for _, u := range v.importUnits(fc, imp.Path) {
			into[u] = true
		}
		if packageScoped[lang] && pkg != "" {
			if *intoPkg == nil {
				*intoPkg = map[string]bool{}
			}
			(*intoPkg)[pkg] = true
		}
	}
	return sc
}

// importScope is what a file's imports bring into scope for one name:
// units (directories), and declared packages in package-scoped languages.
type importScope struct {
	lang                 string
	named, broad         map[string]bool
	namedPkgs, broadPkgs map[string]bool
}

func (sc importScope) hasNamed(s Symbol) bool {
	return sc.named[s.Package] || s.Lang == sc.lang && !s.ParentType && s.PkgName != "" && sc.namedPkgs[s.PkgName]
}

// hasBroad: import a.b.* brings the top-level declarations of a.b.
func (sc importScope) hasBroad(s Symbol) bool {
	return sc.broad[s.Package] && !(packageScoped[sc.lang] && s.ParentType) ||
		s.Lang == sc.lang && !s.ParentType && s.PkgName != "" && sc.broadPkgs[s.PkgName]
}

func (sc importScope) anyNamed() bool { return len(sc.named) > 0 || len(sc.namedPkgs) > 0 }

// declaredPackage reports whether a package-scoped language file declares
// package pkg.
func (v *View) declaredPackage(pkg string) bool { return pkg != "" && v.project().declared[pkg] }

func (v *View) resolveImported(fc *fileCtx, rec segment.RefRec, kinds map[string]bool) ([]Symbol, string) {
	spec := rec.Qualifier
	lang := fc.meta.Lang
	units := map[string]bool{}
	for _, u := range v.importUnits(fc, spec) {
		units[u] = true
	}
	// In package-scoped languages the spec may name a declared package or
	// a class in one (a.b.Helper), wherever its files are.
	_, parent := splitLast(spec)
	pkgs := map[string]bool{}
	if packageScoped[lang] {
		for _, p := range []string{spec, parent} {
			if v.declaredPackage(p) {
				pkgs[p] = true
			}
		}
	}
	if len(units) == 0 && len(pkgs) == 0 {
		return nil, "" // an external package
	}
	// The import may name a type (Java a.b.Helper, Rust a::Helper): its
	// static members come first.
	owner := lastSegment(spec)
	var members, free []Symbol
	collect := func(units map[string]bool) {
		v.eachNamed(rec.Name, func(i, k int) {
			s := v.symbol(i, k)
			if !(units[s.Package] || s.Lang == lang && pkgs[s.PkgName]) || s.Kind == facts.KindEmbed {
				return
			}
			switch {
			case s.Receiver != "" && facts.BaseType(s.Receiver) == owner:
				members = append(members, s)
			case s.Kind != facts.KindMethod && (kinds[s.Kind] || lang == "go"):
				free = append(free, s)
			}
		})
	}
	collect(units)
	if len(members) == 0 && len(free) == 0 && len(units) > 0 && lang != "go" {
		// A re-exported type (use krate::Item where lib.rs has pub use
		// self::model::Item) or ns.f() on a barrel: follow re-exports.
		for _, name := range []string{owner, rec.Name} {
			rx := map[string]bool{}
			for _, t := range v.followReexports(lang, keys(units), name, 0) {
				if t.name == name {
					rx[t.unit] = true
				}
			}
			if len(rx) > 0 {
				collect(rx)
				break
			}
		}
	}
	if len(members) > 0 {
		return capped(v.fitCall(fc, rec.Enclosing, rec, members, 0)), ImportResolved
	}
	if len(free) > 0 {
		return capped(v.fitCall(fc, rec.Enclosing, rec, free, 0)), ImportResolved
	}
	return nil, ""
}

func (v *View) resolveMember(fc *fileCtx, caller int, rec segment.RefRec, kinds map[string]bool) ([]Symbol, string) {
	fit := func(c []Symbol) []Symbol { return v.fitCall(fc, caller, rec, c, 0) }
	if q := strings.TrimSuffix(rec.Qualifier, "()"); (q == "super" || q == "base") && caller >= 0 {
		var ms []Symbol
		for _, parent := range v.supertypes(v.receiverOf(fc.seg, caller)) {
			ms = append(ms, v.methodsOf(parent, rec.Name, 1)...)
		}
		ms = definitionsFirst(fit(ms))
		switch len(ms) {
		case 0:
		case 1:
			return ms, TypeHinted
		default:
			return capped(ms), Ambiguous
		}
	}
	typ, known := v.qualifierType(fc, caller, rec.Qualifier, 0, true)
	if typ != "" {
		if ms := v.methodsOf(typ, rec.Name, 0); len(ms) > 0 {
			ms = v.preferVisible(fc, v.byQualifiedOwner(typ, ms))
			if rec.Kind == facts.RefCall {
				ms = v.inheritedCallables(fc.meta.Lang, typ, rec.Name, preferCallable(ms))
			}
			ms = definitionsFirst(fit(ms))
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
	if typ != "" && rec.Kind == facts.RefCall && fc.meta.Lang == "csharp" {
		// C# extension methods: static methods whose first parameter is
		// "this T". An external type's supertypes are unknown, so any
		// extension of that name is a candidate, labelled by name.
		exact, any := v.extensionMethods(typ, rec.Name)
		if len(exact) > 0 {
			exact = definitionsFirst(v.fitCall(fc, caller, rec, v.preferVisible(fc, exact), 1))
			if len(exact) == 1 {
				return exact, TypeHinted
			}
			return capped(exact), Ambiguous
		}
		if len(any) > 0 {
			return v.labelByName(v.inBuildDeps(fc, v.fitCall(fc, caller, rec, v.preferVisible(fc, any), 1)), fc.meta.Package)
		}
	}
	if typ != "" && rec.Kind == facts.RefCall && fc.meta.Lang == "kotlin" {
		// Kotlin extension functions: fun Foo.f() for Foo or a supertype,
		// or a generic fun <T> T.f() for any receiver.
		if exact, generic := v.kotlinExtensions(typ, rec.Name); len(exact) > 0 {
			return capped(fit(exact)), TypeHinted
		} else if len(generic) > 0 {
			return v.labelByName(fit(generic), fc.meta.Package)
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
	return v.labelByName(v.inBuildDeps(fc, fit(ms)), fc.meta.Package)
}

// extensionMethods returns the C# extension methods named name: exact
// extends typ (or a supertype declared in the workspace), any extends
// some other type.
func (v *View) extensionMethods(typ, name string) (exact, any []Symbol) {
	want := map[string]bool{typeBase(typ): true}
	for _, sup := range v.supertypes(typeBase(typ)) {
		want[typeBase(sup)] = true
	}
	for _, m := range v.named(name, map[string]bool{facts.KindMethod: true}) {
		if m.Lang != "csharp" {
			continue
		}
		i := strings.Index(m.Signature, "(this ")
		if i < 0 {
			continue
		}
		fields := strings.Fields(m.Signature[i+len("(this "):])
		if len(fields) == 0 {
			continue
		}
		if want[typeBase(plainType(fields[0]))] {
			exact = append(exact, m)
		} else {
			any = append(any, m)
		}
	}
	return exact, any
}

// Languages with overloading: a member declared in a subtype hides only
// the supertype members with the same parameters.
var overloading = map[string]bool{"java": true, "csharp": true, "cpp": true, "kotlin": true, "scala": true, "swift": true, "dart": true}

// inheritedCallables completes the members named name found on typ for a
// call. When typ has only a field or property of that name, the methods
// of its supertypes are the candidates (a field childNodes, a method
// childNodes() in the base class). In languages with overloading, the
// supertypes' overloads with other parameter lists are candidates too
// (Document.text(String) does not hide Element.text()).
func (v *View) inheritedCallables(lang, typ, name string, ms []Symbol) []Symbol {
	callable := func(s Symbol) bool {
		return s.Kind == facts.KindMethod || s.Kind == facts.KindFunc || s.Kind == facts.KindConstructor
	}
	have := map[string]bool{}
	anyCallable := false
	for _, m := range ms {
		if callable(m) {
			anyCallable = true
			have[paramsOf(m.Signature, m.Name)] = true
		}
	}
	if anyCallable && !overloading[lang] {
		return ms
	}
	var extra []Symbol
	for _, parent := range v.supertypes(typeBase(typ)) {
		for _, p := range v.methodsOf(parent, name, 1) {
			if !callable(p) {
				continue
			}
			sig := paramsOf(p.Signature, p.Name)
			if anyCallable && have[sig] {
				continue // an override of a member already found
			}
			have[sig] = true
			extra = append(extra, p)
		}
	}
	switch {
	case len(extra) == 0:
		return ms
	case !anyCallable:
		return extra
	}
	return append(slices.Clone(ms), extra...)
}

// paramsOf returns the parameter list of name in a signature, with spaces
// removed: "(Stringtext)" for text(String text).
func paramsOf(sig, name string) string {
	at := strings.Index(sig, name+"(")
	if at < 0 {
		return ""
	}
	rest := sig[at+len(name):]
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return strings.Join(strings.Fields(rest[:i+1]), "")
			}
		}
	}
	return rest
}

// preferCallable keeps the methods, functions and constructors among a
// call's candidate members when there are any: x.depth() calls the method,
// not the field of the same name (a field holding a function stays a
// candidate when nothing else is).
func preferCallable(ms []Symbol) []Symbol {
	if len(ms) <= 1 {
		return ms
	}
	if in := filter(ms, func(s Symbol) bool {
		return s.Kind == facts.KindMethod || s.Kind == facts.KindFunc || s.Kind == facts.KindConstructor
	}); len(in) > 0 {
		return in
	}
	return ms
}

// byName labels a name-only match: name_matched for one candidate,
// ambiguous otherwise (same-unit candidates first).
func (v *View) labelByName(cands []Symbol, unit string) ([]Symbol, string) {
	cands = definitionsFirst(cands)
	if len(cands) == 1 {
		return cands, NameMatched
	}
	cands = slices.Clone(cands) // named results are shared
	slices.SortStableFunc(cands, func(a, b Symbol) int {
		ai, bi := a.Package == unit, b.Package == unit
		switch {
		case ai && !bi:
			return -1
		case bi && !ai:
			return 1
		}
		// Ruby autoloading (Zeitwerk, Rails): UsersController lives in
		// users_controller.rb.
		if a.Lang == "ruby" && b.Lang == "ruby" {
			ar, br := autoloaded(a), autoloaded(b)
			switch {
			case ar && !br:
				return -1
			case br && !ar:
				return 1
			}
		}
		return 0
	})
	return capped(cands), Ambiguous
}

// autoloaded reports whether s is declared in the file Ruby autoloading
// expects for its name (snake_case of a constant).
func autoloaded(s Symbol) bool {
	return startsUpper(s.Name) && stripExt(path.Base(s.File)) == snakeCase(s.Name)
}

// snakeCase turns a CamelCase constant into snake_case (HTTPClient ->
// http_client).
func snakeCase(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		upper := c >= 'A' && c <= 'Z'
		if upper && i > 0 {
			prevLower := name[i-1] >= 'a' && name[i-1] <= 'z' || name[i-1] >= '0' && name[i-1] <= '9'
			nextLower := i+1 < len(name) && name[i+1] >= 'a' && name[i+1] <= 'z'
			if prevLower || (nextLower && name[i-1] >= 'A' && name[i-1] <= 'Z') {
				b.WriteByte('_')
			}
		}
		if upper {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// preferVisible keeps the candidates declared nearest to the file: in the
// file itself, else in its unit, else in its unit or imported units, when
// that leaves any (a same-file or same-package type shadows one reached by
// a wildcard import).
//
// Members are ranked by their owner type as the file sees its name: an
// owner declared in the file, then one an import names (Java's
// single-type import shadows the package), then a top-level owner in the
// file's unit, then one a wildcard or module import brings.
func (v *View) preferVisible(fc *fileCtx, cands []Symbol) []Symbol {
	if len(cands) <= 1 {
		return cands
	}
	if in := filter(cands, func(s Symbol) bool { return s.File == fc.meta.Path }); len(in) > 0 {
		return in
	}
	if !packageScoped[fc.meta.Lang] {
		if in := filter(cands, func(s Symbol) bool { return v.sameUnit(fc, s) }); len(in) > 0 {
			return in
		}
		return v.inImportedUnits(fc, cands)
	}
	scopes := map[string]importScope{}
	owner := func(s Symbol) (Symbol, importScope) {
		o, ok := v.ownerOf(s)
		if !ok {
			o = s
		}
		sc, done := scopes[o.Name]
		if !done {
			sc = v.importedUnitsFor(fc, o.Name)
			scopes[o.Name] = sc
		}
		return o, sc
	}
	if in := filter(cands, func(s Symbol) bool { o, sc := owner(s); return sc.hasNamed(o) }); len(in) > 0 {
		return in
	}
	if in := filter(cands, func(s Symbol) bool { o, _ := owner(s); return v.sameUnit(fc, o) }); len(in) > 0 {
		return in
	}
	if in := filter(cands, func(s Symbol) bool { o, sc := owner(s); return sc.hasBroad(o) }); len(in) > 0 {
		return in
	}
	return v.inImportedUnits(fc, cands)
}

// inImportedUnits keeps the candidates in the file's unit or a unit it
// imports, when any are.
func (v *View) inImportedUnits(fc *fileCtx, cands []Symbol) []Symbol {
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

// ownerOf returns the type declaring member s (the type named by its
// receiver, in its file), or s itself when it is not a member.
func (v *View) ownerOf(s Symbol) (Symbol, bool) {
	if s.Receiver == "" {
		return s, true
	}
	base := facts.BaseType(s.Receiver)
	var found Symbol
	ok := false
	v.eachNamed(base, func(i, k int) {
		if ok {
			return
		}
		if o := v.symbol(i, k); o.File == s.File && isTypeKind(o.Kind) {
			found, ok = o, true
		}
	})
	return found, ok
}

// byQualifiedOwner narrows members of a qualified type (Connection.Request,
// org.x.Foo) to those whose owner is nested in the qualifier's type or
// declared in the qualifier's package, when any are.
func (v *View) byQualifiedOwner(typ string, ms []Symbol) []Symbol {
	_, qual := splitLast(typ)
	if qual == "" || len(ms) < 2 {
		return ms
	}
	outer := lastSegment(qual)
	in := filter(ms, func(m Symbol) bool {
		o, ok := v.ownerOf(m)
		if !ok {
			return false
		}
		return o.Parent == outer && o.ParentType || o.PkgName == qual || strings.HasSuffix(o.PkgName, "."+qual)
	})
	if len(in) == 0 {
		return ms
	}
	return in
}

// named returns the live declarations called name of the given kinds, in
// symbol order. Each name is decoded once per view, and filtered once per
// kind set. Callers must not modify the result.
func (v *View) named(name string, kinds map[string]bool) []Symbol {
	key := namedKey{name, reflect.ValueOf(kinds).Pointer()}
	if out, ok := v.byKinds[key]; ok {
		return out
	}
	out := filter(v.allNamed(name), func(s Symbol) bool { return kinds[s.Kind] })
	if v.byKinds == nil {
		v.byKinds = map[namedKey][]Symbol{}
	}
	v.byKinds[key] = out
	return out
}

// namedKey identifies a name and a kind set (by map identity).
type namedKey struct {
	name  string
	kinds uintptr
}

// allNamed returns every live declaration called name except embeds.
func (v *View) allNamed(name string) []Symbol {
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
	return all
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
//
// counted says the call segments keep their arguments (a reference's
// qualifier), so overloads can be told apart by argument count; hint
// chains have their arguments stripped.
func (v *View) qualifierType(fc *fileCtx, caller int, q string, depth int, counted bool) (typ string, known bool) {
	if q == "" || depth > maxTypeDepth {
		return "", false
	}
	args := func(seg string) argCount {
		if !counted {
			return argCount{}
		}
		return segmentArgs(seg)
	}
	segs := splitQualifier(q)
	first := segs[0]
	switch {
	case castType(first) != "":
		typ = castType(first)
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
		if name, ok := callSegment(first); ok && typ == "" {
			typ = v.callType(name, args(first), depth)
		}
	}
	if typ == "" {
		return "", false
	}
	for _, seg := range segs[1:] {
		var next string
		if name, ok := callSegment(seg); ok {
			next = v.memberCallType(typ, name, args(seg), depth)
		} else {
			next = v.fieldType(typ, seg, depth)
		}
		if next == "" {
			return "", false
		}
		typ = next
	}
	return typ, true
}

// castType returns the type of a parenthesized cast used as a receiver:
// ((T) x) in C-family languages, (x as T) in TypeScript, C# and Kotlin.
func castType(seg string) string {
	if !strings.HasPrefix(seg, "(") || !strings.HasSuffix(seg, ")") {
		return ""
	}
	inner := strings.TrimSpace(seg[1 : len(seg)-1])
	if i := strings.LastIndex(inner, " as "); i > 0 {
		return plainType(strings.TrimSuffix(strings.TrimSpace(inner[i+4:]), "?"))
	}
	if !strings.HasPrefix(inner, "(") {
		return ""
	}
	end := strings.IndexByte(inner, ')')
	if end < 0 || strings.TrimSpace(inner[end+1:]) == "" {
		return "" // ((x)) or a call, not a cast
	}
	t := strings.TrimSpace(inner[1:end])
	for _, r := range t {
		if !isIdentRune(r) && !strings.ContainsRune(".:<>*&, ", r) {
			return ""
		}
	}
	return plainType(strings.TrimPrefix(t, "const "))
}

// isIdentRune reports ASCII identifier characters.
func isIdentRune(r rune) bool {
	return r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// callSegment returns the callee of a call segment "f(args)" or
// "f::<T>(args)".
func callSegment(s string) (string, bool) {
	if !strings.HasSuffix(s, ")") {
		return "", false
	}
	i := strings.IndexByte(s, '(')
	if i <= 0 {
		return "", false
	}
	name := s[:i]
	if j := strings.Index(name, "::<"); j > 0 {
		name = name[:j]
	}
	return name, true
}

// memberCallType returns the type a call of member name on a value of
// type typ returns (builder chains: T::new(x).m(y).n()); T::new() without
// a declared new constructs T.
func (v *View) memberCallType(typ, name string, args argCount, depth int) string {
	if depth > maxTypeDepth {
		return ""
	}
	for _, m := range args.fit(v.methodsOf(typ, name, 0)) {
		switch rt := resultType(m.Signature, m.Name); rt {
		case "":
			continue
		case "Self", "self", "this", "instancetype":
			return facts.BaseType(m.Receiver)
		default:
			if isTypeParam(m.Signature, m.Name, rt) {
				return ""
			}
			return rt
		}
	}
	if name == "new" {
		return typ
	}
	return ""
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
			if packageScoped[fc.meta.Lang] {
				_, parent := splitLast(imp.Path)
				if v.declaredPackage(imp.Path) || v.declaredPackage(parent) {
					return false
				}
			}
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
	if !strings.HasSuffix(t, ")") {
		return t
	}
	if _, qual := splitLast(t); strings.Contains(qual, "(") {
		// A call chain: T::new(a).m(b) or f().g().
		typ, _ := v.qualifierType(fc, -1, t, depth+1, false)
		return typ
	}
	name, _ := callSegment(t)
	return v.callType(name, argCount{}, depth+1)
}

// callType returns the type a call to callee (f, pkg.f, T::new) returns.
func (v *View) callType(callee string, args argCount, depth int) string {
	if depth > maxTypeDepth {
		return ""
	}
	name, qual := splitLast(callee)
	if name == "new" && qual != "" {
		return qual
	}
	for _, s := range args.fit(v.named(name, callResultKinds)) {
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
			if isTypeParam(s.Signature, s.Name, rt) {
				return "" // mock<T>(): T depends on the call
			}
			return rt
		}
	}
	return ""
}

var callResultKinds = map[string]bool{facts.KindFunc: true, facts.KindMethod: true, facts.KindClass: true, facts.KindStruct: true, facts.KindConstructor: true}

// argCount is the argument count of a call segment in a receiver
// expression, when known.
type argCount struct {
	n  int
	ok bool
}

// fit keeps the declarations in languages with overloading that accept
// the count, when some do (overloads can return different types).
func (a argCount) fit(cands []Symbol) []Symbol {
	if !a.ok || len(cands) < 2 {
		return cands
	}
	in := filter(cands, func(s Symbol) bool { return !overloading[s.Lang] || s.Params.Accepts(a.n) })
	if len(in) == 0 {
		return cands
	}
	return in
}

// segmentArgs counts the arguments of a call segment "f(a, b)" of a
// receiver expression. A truncated qualifier ("...") or unbalanced text
// gives an unknown count.
func segmentArgs(seg string) argCount {
	open := strings.IndexByte(seg, '(')
	if open < 0 || !strings.HasSuffix(seg, ")") || strings.Contains(seg, "...") {
		return argCount{}
	}
	inner := strings.TrimSpace(seg[open+1 : len(seg)-1])
	if inner == "" {
		return argCount{0, true}
	}
	n, depth := 1, 0
	for i := 0; i < len(inner); i++ {
		switch c := inner[i]; c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth--; depth < 0 {
				return argCount{}
			}
		case '"', '\'':
			j := strings.IndexByte(inner[i+1:], c)
			if j < 0 {
				return argCount{}
			}
			i += j + 1
		case ',':
			if depth == 0 {
				n++
			}
		}
	}
	if depth != 0 {
		return argCount{}
	}
	return argCount{n, true}
}

// isTypeParam reports whether t is a type parameter declared in the
// signature of name: in a <...> list before the name (Java, Kotlin, C++
// templates) or right after it (TypeScript, C#, Rust, Swift).
func isTypeParam(sig, name, t string) bool {
	at := strings.Index(sig, name+"(")
	if at < 0 {
		at = strings.Index(sig, name+"<")
	}
	if at < 0 {
		return false
	}
	head := sig[:at]
	if rest := sig[at+len(name):]; strings.HasPrefix(rest, "<") {
		if end := strings.IndexByte(rest, '('); end > 0 {
			head += " " + rest[:end]
		}
	}
	for {
		i := strings.IndexByte(head, '<')
		if i < 0 {
			return false
		}
		depth, j := 0, i
		for ; j < len(head); j++ {
			if head[j] == '<' {
				depth++
			} else if head[j] == '>' {
				if depth--; depth == 0 {
					break
				}
			}
		}
		for _, w := range strings.FieldsFunc(head[i+1:min(j, len(head))], func(r rune) bool { return !isIdentRune(r) }) {
			if w == t {
				return true
			}
		}
		if j >= len(head) {
			return false
		}
		head = head[j+1:]
	}
}

// resultType extracts the declared result type from a signature: Go's
// results after the parameters, "-> T" (Python, Rust, Swift), "): T"
// (TypeScript, Kotlin, Scala, PHP), or the word before the name (Java, C#,
// C, C++, Dart).
func resultType(sig, name string) string {
	raw, before := resultTypeRaw(sig, name)
	if f := strings.Fields(raw); before && len(f) > 0 {
		raw = f[len(f)-1] // the word before the name (Java, C#, C, C++, Dart)
	}
	return plainType(raw)
}

// resultTypeRaw is resultType's text before plainType: Go's results after
// the parameters, "-> T", "): T", or (before) every word before the name.
func resultTypeRaw(sig, name string) (string, bool) {
	at := strings.Index(sig, name+"(")
	if at < 0 {
		at = strings.Index(sig, name+"<")
	}
	if at < 0 {
		return "", false
	}
	open := strings.IndexByte(sig[at:], '(')
	if open < 0 {
		return "", false
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
		return "", false
	}
	rest := strings.TrimSpace(sig[close+1:])
	switch {
	case strings.HasPrefix(rest, "->"):
		return strings.TrimSpace(rest[2:]), false
	case strings.HasPrefix(rest, ":"):
		return strings.TrimSpace(rest[1:]), false
	case strings.HasPrefix(sig, "func ") || strings.HasPrefix(sig, "func("):
		if strings.HasPrefix(rest, "(") {
			rest = rest[1:]
			if j := strings.IndexAny(rest, ",)"); j >= 0 {
				rest = rest[:j]
			}
		}
		return strings.TrimSpace(rest), false
	}
	before := strings.Fields(sig[:at])
	if len(before) == 0 {
		return "", false
	}
	// the words before the name: const char* in C, unsigned int
	return strings.Join(before, " "), true
}

// plainType strips pointer, reference, nullable, array and generic syntax
// from a type as written; "" when nothing named remains.
func plainType(t string) string {
	t = unwrapOptional(strings.Trim(strings.TrimSpace(t), "\"'"))
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

// unwrapOptional returns X for Optional[X], t.Optional[X], X | None and
// None | X, with quotes removed from forward references ("X").
func unwrapOptional(t string) string {
	for _, p := range []string{"typing.Optional[", "t.Optional[", "Optional["} {
		if strings.HasPrefix(t, p) && strings.HasSuffix(t, "]") {
			t = t[len(p) : len(t)-1]
		}
	}
	if a, b, ok := strings.Cut(t, "|"); ok {
		a, b = strings.TrimSpace(a), strings.TrimSpace(b)
		switch {
		case b == "None" || b == "null" || b == "undefined":
			t = a
		case a == "None" || a == "null" || a == "undefined":
			t = b
		}
	}
	return strings.Trim(strings.TrimSpace(t), "\"'")
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
	// Relative specs, and JS/TS specs (the nearest tsconfig applies),
	// depend on the importing directory.
	relative := strings.HasPrefix(spec, ".") || lang == "ruby" || lang == "shell" || lang == "c" || lang == "cpp" || lang == "objc" || lang == "dart" ||
		lang == "javascript" || lang == "typescript" ||
		(lang == "rust" && (strings.HasPrefix(spec, "crate::") || strings.HasPrefix(spec, "self::") || strings.HasPrefix(spec, "super::")))
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
			return v.jsPackageUnits(dir, spec) // tsconfig paths, workspace packages; else external
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
		if u := v.pythonRootUnits(p); len(u) > 0 {
			return u
		}
		cands = []string{p}
	case "rust":
		segs := strings.Split(spec, "::")
		switch segs[0] {
		case "crate":
			if root, ok := v.rustCrateRoot(dir); ok {
				// Exact paths under the crate root; crate::Name names an item
				// of the root module itself.
				base := path.Join(append([]string{root}, segs[1:]...)...)
				var out []string
				for _, p := range []string{base, path.Dir(base)} {
					out = append(out, v.dirUnits(p, true)...)
					out = append(out, v.fileUnits([]string{p}, true)...)
				}
				if len(out) > 0 {
					return uniq(out)
				}
			}
			segs = segs[1:]
		case "self":
			segs = append(strings.Split(dir, "/"), segs[1:]...)
		case "super":
			segs = append(strings.Split(path.Dir(dir), "/"), segs[1:]...)
		default:
			if u := v.rustCrateUnits(segs); len(u) > 0 {
				return u // a workspace crate
			}
		}
		cands = []string{strings.Join(segs, "/")}
	case "ruby", "shell", "c", "cpp", "objc":
		rel := strings.TrimPrefix(spec, "./")
		local := path.Join(dir, rel)
		if u := v.fileUnits([]string{stripExt(local)}, true); len(u) > 0 {
			return u
		}
		if lang == "c" || lang == "cpp" || lang == "objc" {
			if u := v.includeUnits(rel); len(u) > 0 {
				return u // an include directory of compile_commands.json
			}
		}
		return v.fileUnits([]string{stripExt(rel)}, false)
	case "dart":
		if u, ok := v.dartPackageUnits(spec); ok && len(u) > 0 {
			return u
		}
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
		if u := v.phpUnits(spec); len(u) > 0 {
			return u // composer PSR-4
		}
		cands = []string{strings.ReplaceAll(strings.Trim(spec, "\\"), "\\", "/")}
	case "swift":
		if u := v.swiftModuleUnits(spec); len(u) > 0 {
			return u
		}
		cands = []string{strings.ReplaceAll(spec, ".", "/")}
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

// rustCrateRoot returns the directory of the crate root (lib.rs or
// main.rs) nearest above dir.
func (v *View) rustCrateRoot(dir string) (string, bool) {
	for d := dir; ; d = path.Dir(d) {
		for _, root := range []string{"lib.rs", "main.rs"} {
			if _, ok := v.sn.Lookup(path.Join(d, root)); ok {
				return d, true
			}
		}
		if d == "." || d == "/" || d == "" {
			return "", false
		}
	}
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

// IsTest reports whether s is a test function or method, as the extractor
// marked it: Go test, example, fuzz and benchmark functions in _test.go
// files, and each pack's test-name globs and markers (JUnit @Test, Rust
// #[test], pytest test_*, ...).
func IsTest(s Symbol) bool { return s.Test }

// ResolvedRef is a reference with its resolution, for evaluation tools.
type ResolvedRef struct {
	File       string
	Line, Col  int // 1-based; Col is a byte column
	Name       string
	Kind       uint8 // facts.Ref* constant
	Targets    []Symbol
	Resolution string
}

// EachReference resolves every live reference of the given kinds and calls
// fn with each, in segment order. It is meant for offline evaluation (the
// SCIP edge baseline), not for the query path.
func (v *View) EachReference(kinds []uint8, fn func(ResolvedRef)) {
	for i := 0; i < v.sn.NumSegments(); i++ {
		seg := v.sn.Segment(i)
		for r := 0; r < seg.NumRefs(); r++ {
			kind := seg.RefKind(r)
			if !slices.Contains(kinds, kind) || !v.sn.Live(i, seg.RefFile(r)) {
				continue
			}
			rec := seg.Ref(r)
			targets, label := v.resolveRef(i, r)
			fn(ResolvedRef{
				File: v.meta(i, rec.File).Path, Line: rec.Line, Col: rec.Col, Name: rec.Name,
				Kind: kind, Targets: targets, Resolution: label,
			})
		}
	}
}
