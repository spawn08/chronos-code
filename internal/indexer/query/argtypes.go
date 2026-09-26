package query

import (
	"strconv"
	"strings"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/segment"
)

// Overload selection by argument types (M7). A call's arguments carry
// cheap type hints (facts.Ref.ArgTypes: literal classes, identifiers,
// constructed types); each overload's parameter types are read from its
// signature. An overload with an argument that cannot convert to its
// parameter is dropped, and the best exact matches are kept.

// Parameter type families.
const (
	famUnknown = iota
	famString
	famNumber
	famBool
	famChar
	famAny     // Object, Any, id, void*
	famGeneric // a type parameter
	famClass   // a named type (typ)
	famFunc    // a function type, lambda or function reference
)

// Match scores of one argument against one parameter.
const (
	scoreNo      = -1 // cannot be passed
	scoreUnknown = 0
	scoreConvert = 1 // an implicit conversion (C++ int to bool, char to int)
	scoreExact   = 2
)

// fitCall narrows the candidates of a call by argument count, then by
// argument types. extra is added to argument positions (C# extension
// methods' this parameter).
func (v *View) fitCall(fc *fileCtx, caller int, rec segment.RefRec, cands []Symbol, extra int) []Symbol {
	cands = v.byArgTypes(fc, caller, rec, byArity(fc.meta.Lang, rec, cands, extra), extra)
	if fc.meta.Lang == "kotlin" && rec.QualKind == facts.QualNone && len(cands) > 1 {
		// An extension function (fun R.f()) needs a receiver: an
		// unqualified call has one only inside an extension of R or a
		// member of R, and then R's extensions come first.
		recv := ""
		if caller >= 0 {
			if c := v.symbol(fc.seg, caller); isExtension(c) {
				recv = extensionReceiver(c)
			} else {
				recv = v.receiverOf(fc.seg, caller)
			}
		}
		if recv != "" {
			if in := filter(cands, func(s Symbol) bool {
				r := extensionReceiver(s)
				return isExtension(s) && r != "" && (r == recv || v.isSubtype(recv, r, 0))
			}); len(in) > 0 {
				return in
			}
		}
		if in := filter(cands, func(s Symbol) bool { return !isExtension(s) }); len(in) > 0 {
			return in
		}
	}
	return cands
}

// isExtension reports a Kotlin extension function: a receiver type
// between "fun" (and its type parameters) and the name.
func isExtension(s Symbol) bool {
	if s.Lang != "kotlin" || s.Receiver != "" {
		return false
	}
	at := strings.Index(s.Signature, "fun ")
	name := strings.Index(s.Signature, " "+s.Name+"(")
	if name < 0 {
		name = strings.Index(s.Signature, "."+s.Name+"(")
	}
	if at < 0 || name < at {
		return false
	}
	head := s.Signature[at+4 : name+1]
	if strings.HasPrefix(head, "<") {
		depth := 0
		for i := 0; i < len(head); i++ {
			if head[i] == '<' {
				depth++
			} else if head[i] == '>' {
				if depth--; depth == 0 {
					head = head[i+1:]
					break
				}
			}
		}
	}
	return strings.HasSuffix(strings.TrimSpace(head), ".") || strings.Contains(s.Signature[at:], "."+s.Name+"(")
}

// byArgTypes keeps the overloads that best match the call's argument type
// hints, in languages with overloading, when that leaves any.
func (v *View) byArgTypes(fc *fileCtx, caller int, rec segment.RefRec, cands []Symbol, extra int) []Symbol {
	lang := fc.meta.Lang
	nargs, counted := rec.NArgs()
	if len(cands) < 2 || !counted || !overloading[lang] {
		return cands
	}
	// Without hints the arguments are unknowns: binding still checks
	// required and named parameters.
	hints := make([]string, nargs)
	if rec.ArgTypes != "" {
		hints = strings.Split(rec.ArgTypes, ",")
	}
	args := make([]argType, len(hints))
	for i, h := range hints {
		args[i] = v.argTypeOf(fc, caller, h)
	}
	// As in byArity, a definition accepts what a bodiless declaration of
	// the same member with as many parameters accepts (C++ defaults).
	n := len(args) + extra
	declared := map[string]bool{}
	for _, c := range cands {
		if c.Decl && c.Params.Known && c.Params.Accepts(n) {
			declared[c.Kind+"\x00"+c.Qualified()+"\x00"+itoaInt(c.Params.Max)] = true
		}
	}
	best, bestScore := []Symbol(nil), -1
	for _, c := range cands {
		own := !c.Params.Known || c.Params.Accepts(n)
		accepted := own || declared[c.Kind+"\x00"+c.Qualified()+"\x00"+itoaInt(c.Params.Max)]
		score, ok := v.matchParams(c, args, extra, accepted, own && c.Params.Known)
		switch {
		case !ok:
		case score > bestScore:
			best, bestScore = []Symbol{c}, score
		case score == bestScore:
			best = append(best, c)
		}
	}
	if len(best) == 0 {
		return cands
	}
	return best
}

// argType is what is known about one argument's type.
type argType struct {
	fam      int
	typ      string // for famClass: the type's base name
	name     string // a named argument's name
	trailing bool   // a trailing lambda: binds the last parameter
}

func (v *View) argTypeOf(fc *fileCtx, caller int, hint string) argType {
	if eq := strings.IndexByte(hint, '='); eq > 0 {
		a := v.argTypeOf(fc, caller, hint[eq+1:])
		a.name = hint[:eq]
		return a
	}
	switch hint {
	case "":
		return argType{}
	case "#f":
		return argType{fam: famFunc}
	case "#t":
		return argType{fam: famFunc, trailing: true}
	case "#s":
		return argType{fam: famString}
	case "#n":
		return argType{fam: famNumber}
	case "#b":
		return argType{fam: famBool}
	case "#c":
		return argType{fam: famChar}
	case "#0":
		return argType{fam: famAny, typ: "null"}
	}
	typ := ""
	if t, ok := strings.CutPrefix(hint, "@"); ok {
		typ = t
	} else if callee, ok := strings.CutPrefix(hint, "()"); ok {
		return v.callResultArg(callee)
	} else {
		typ = v.hintType(fc, caller, hint, 0)
		if typ == "" {
			// An enum constant (XML_SUCCESS) has its enum's type.
			for _, m := range v.named(hint, map[string]bool{facts.KindEnumMember: true}) {
				if m.Parent != "" {
					typ = m.Parent
					break
				}
			}
		}
	}
	if typ == "" {
		return argType{}
	}
	if typ == "->" {
		return argType{fam: famFunc}
	}
	if typ == "char" || typ == "wchar_t" {
		return argType{} // hints drop pointers: char may be char*
	}
	fam, base := typeFamily(typ)
	return argType{fam: fam, typ: base}
}

// callResultArg types an argument that is a call to callee by the result
// type its declarations agree on (a constructor call has its class).
func (v *View) callResultArg(callee string) argType {
	cands := v.named(callee, callResultKinds)
	// Free functions, types and constructors first: a call argument is
	// rarely an unqualified method of another class.
	if free := filter(cands, func(s Symbol) bool { return s.Receiver == "" && !isExtension(s) }); len(free) > 0 {
		cands = free
	}
	var out argType
	for i, s := range cands {
		var a argType
		switch {
		case isTypeKind(s.Kind):
			a = argType{fam: famClass, typ: s.Name}
		case s.Kind == facts.KindConstructor:
			a = argType{fam: famClass, typ: facts.BaseType(s.Receiver)}
		default:
			raw, before := resultTypeRaw(s.Signature, s.Name)
			if before {
				raw = resultWords(raw)
			}
			switch raw {
			case "":
				return argType{}
			case "Self", "self", "this", "instancetype":
				a = argType{fam: famClass, typ: facts.BaseType(s.Receiver)}
			default:
				fam, base := typeFamily(raw)
				if fam == famClass && isTypeParam(s.Signature, s.Name, base) {
					return argType{}
				}
				a = argType{fam: fam, typ: base}
			}
		}
		if i > 0 && a != out {
			return argType{} // overloads that disagree
		}
		out = a
	}
	return out
}

// resultWords drops the words before a name that are not its result type:
// modifiers, annotations and a C++ scope (XMLDocument::).
func resultWords(raw string) string {
	var keep []string
	for _, w := range strings.Fields(raw) {
		switch {
		case strings.HasPrefix(w, "@"), strings.HasSuffix(w, "::"), strings.HasPrefix(w, "["):
		case resultModifiers[w]:
		default:
			keep = append(keep, w)
		}
	}
	return strings.Join(keep, " ")
}

var resultModifiers = map[string]bool{
	"public": true, "private": true, "protected": true, "internal": true, "static": true, "virtual": true,
	"inline": true, "final": true, "override": true, "abstract": true, "synchronized": true, "explicit": true,
	"constexpr": true, "extern": true, "async": true, "suspend": true, "fun": true, "def": true, "func": true,
	"open": true, "sealed": true, "native": true, "default": true, "unsafe": true, "new": true, "partial": true,
}

// matchParams binds args to c's parameters and scores them; ok is false
// when an argument cannot be passed, a named argument names no parameter,
// or a required parameter is left unbound. Positional arguments bind in
// order (from extra), named ones by name (Swift: by label), and a
// trailing lambda binds the last parameter. accepted is false when the
// argument count already ruled c out: it scores 0 and stays only if every
// candidate is out. own says c's own arity accepts the count (not only a
// C++ declaration's defaults), so its required parameters can be checked.
func (v *View) matchParams(c Symbol, args []argType, extra int, accepted, own bool) (int, bool) {
	if !accepted {
		return 0, true
	}
	params := paramsFor(c)
	if params == nil {
		return 0, true
	}
	swift := c.Lang == "swift"
	bound := make([]bool, len(params))
	next, total := extra, 0
	for _, a := range args {
		k := -1
		switch {
		case a.name != "":
			for i, p := range params {
				if (swift && p.label == a.name) || (!swift && p.name == a.name) {
					k = i
					break
				}
			}
			if k < 0 {
				return 0, false
			}
		case a.trailing && (c.Lang == "kotlin" || swift || c.Lang == "scala"):
			k = len(params) - 1
		default:
			for next < len(params) && bound[next] && !params[next].variadic {
				next++
			}
			if next >= len(params) {
				continue // extra arguments: the arity check decides
			}
			k = next
			if !params[k].variadic {
				next++
			}
			if swift && params[k].label != "" {
				return 0, false // a labelled parameter needs its label
			}
		}
		if k < 0 || k >= len(params) {
			continue
		}
		s := v.matchArg(a, params[k].typ, c)
		if s == scoreNo {
			return 0, false
		}
		total += s
		bound[k] = true
	}
	if own {
		for i, p := range params {
			if i >= extra && !bound[i] && !p.optional && !p.variadic {
				return 0, false
			}
		}
	}
	return total, true
}

func (v *View) matchArg(a argType, p string, c Symbol) int {
	if a.fam == famUnknown {
		return scoreUnknown
	}
	pf, pbase := typeFamily(p)
	if isFuncType(p) {
		pf = famFunc
	}
	if a.fam == famFunc || pf == famFunc {
		return funcMatch(a.fam, pf)
	}
	if pf == famClass && isTypeParam(c.Signature, c.Name, pbase) {
		pf = famGeneric
	}
	cpp := c.Lang == "cpp" || c.Lang == "c"
	switch pf {
	case famUnknown:
		return scoreUnknown
	case famGeneric:
		return scoreConvert
	case famAny:
		switch {
		case a.fam == famAny && a.typ != "null":
			return scoreExact // object to object
		case a.fam == famAny:
			return scoreConvert // null
		}
		return scoreUnknown // viable, but the least specific match
	}
	switch a.fam {
	case famString:
		switch pf {
		case famString:
			return scoreExact
		case famClass:
			if charSequence[pbase] {
				return scoreConvert
			}
		}
		return scoreNo
	case famNumber:
		switch pf {
		case famNumber:
			return scoreExact
		case famBool, famChar:
			if cpp {
				return scoreConvert
			}
		case famClass:
			if boxedNumber[pbase] {
				return scoreConvert
			}
		}
		return scoreNo
	case famBool:
		switch pf {
		case famBool:
			return scoreExact
		case famNumber:
			if cpp {
				return scoreConvert
			}
		case famClass:
			if pbase == "Boolean" {
				return scoreConvert
			}
		}
		return scoreNo
	case famChar:
		switch pf {
		case famChar:
			return scoreExact
		case famNumber:
			return scoreConvert
		case famClass:
			if pbase == "Character" {
				return scoreConvert
			}
		}
		return scoreNo
	case famAny:
		if a.typ != "null" { // a value declared object, Object or Any
			if pf == famClass || pf == famString || pf == famNumber || pf == famBool || pf == famChar {
				return scoreNo // needs a cast
			}
			return scoreExact
		}
		switch pf {
		case famNumber, famBool, famChar:
			if cpp {
				return scoreConvert // NULL is 0
			}
			return scoreNo
		}
		return scoreConvert
	case famClass:
		switch pf {
		case famClass:
			if pbase == a.typ {
				return scoreExact
			}
			if v.isSubtype(a.typ, pbase, 0) {
				return scoreExact
			}
			return scoreUnknown // supertypes outside the workspace are unknown
		case famNumber, famBool:
			if cpp && v.isEnum(a.typ) {
				return scoreConvert // unscoped enums convert to integers
			}
			if v.workspaceType(a.typ) {
				return scoreNo
			}
		case famString:
			if v.workspaceType(a.typ) {
				return scoreNo
			}
		}
		return scoreUnknown
	}
	return scoreUnknown
}

// isSubtype reports whether type t extends or implements p, through up
// to maxTypeDepth levels of workspace supertypes.
func (v *View) isSubtype(t, p string, depth int) bool {
	if depth > maxTypeDepth {
		return false
	}
	for _, sup := range v.supertypes(typeBase(t)) {
		if b := typeBase(sup); b == p || v.isSubtype(b, p, depth+1) {
			return true
		}
	}
	return false
}

// funcMatch scores a lambda against a parameter, or any argument against
// a function-typed parameter.
func funcMatch(arg, param int) int {
	switch {
	case arg == famFunc && param == famFunc:
		return scoreExact
	case param == famGeneric || param == famAny:
		return scoreConvert
	case arg == famFunc && param == famClass, arg == famClass && param == famFunc:
		return scoreUnknown // a functional interface (SAM), or a class implementing a function type
	case arg == famAny && param == famFunc:
		return scoreConvert // null to a nullable function type
	case param == famUnknown || arg == famUnknown:
		return scoreUnknown
	}
	return scoreNo
}

// isFuncType reports a function type as written: (Int) -> Unit,
// T.() -> Boolean, (x: T) => void, std::function<...>.
func isFuncType(t string) bool {
	return strings.Contains(t, "->") || strings.Contains(t, "=>") || strings.Contains(t, "std::function")
}

func (v *View) workspaceType(name string) bool { return len(v.named(name, typeKinds)) > 0 }

func (v *View) isEnum(name string) bool {
	return len(v.named(name, map[string]bool{facts.KindEnum: true})) > 0
}

var (
	charSequence = map[string]bool{"CharSequence": true, "Comparable": true, "Serializable": true, "String": true}
	boxedNumber  = map[string]bool{"Integer": true, "Long": true, "Short": true, "Byte": true, "Double": true,
		"Float": true, "Number": true, "BigDecimal": true, "BigInteger": true}
	numberWords = map[string]bool{
		"int": true, "long": true, "short": true, "byte": true, "float": true, "double": true, "size_t": true,
		"ssize_t": true, "unsigned": true, "signed": true, "decimal": true, "sbyte": true, "ushort": true,
		"uint": true, "ulong": true, "number": true, "bigint": true, "Int": true, "Long": true, "Short": true,
		"Byte": true, "Double": true, "Float": true, "UInt": true, "ULong": true, "CGFloat": true,
		"int8_t": true, "int16_t": true, "int32_t": true, "int64_t": true, "uint8_t": true, "uint16_t": true,
		"uint32_t": true, "uint64_t": true, "intptr_t": true, "uintptr_t": true, "ptrdiff_t": true,
		"i8": true, "i16": true, "i32": true, "i64": true, "i128": true, "isize": true,
		"u8": true, "u16": true, "u32": true, "u64": true, "u128": true, "usize": true, "f32": true, "f64": true,
		"Int8": true, "Int16": true, "Int32": true, "Int64": true, "UInt8": true, "UInt16": true, "UInt32": true, "UInt64": true,
	}
	stringWords = map[string]bool{"String": true, "string": true, "str": true, "wstring": true, "string_view": true,
		"NSString": true, "Substring": true}
)

// typeFamily classifies a type as written (the parameter's type, or a
// hint's type name) and returns its base name.
func typeFamily(t string) (int, string) {
	t = strings.TrimSpace(t)
	if t == "" {
		return famUnknown, ""
	}
	compact := strings.ReplaceAll(t, " ", "")
	if strings.Contains(compact, "char*") || strings.Contains(compact, "char[]") && !strings.Contains(compact, "unsigned") {
		return famString, "char*"
	}
	if strings.Contains(compact, "void*") {
		return famAny, "void*"
	}
	words := strings.FieldsFunc(t, func(r rune) bool { return !isIdentRune(r) })
	for _, w := range words {
		switch {
		case stringWords[w]:
			return famString, w
		case w == "bool" || w == "boolean" || w == "Bool":
			return famBool, w
		case w == "char" || w == "wchar_t" || w == "Char" || w == "char16_t" || w == "char32_t":
			return famChar, w
		case numberWords[w]:
			return famNumber, w
		case w == "Object" || w == "Any" || w == "object" || w == "id" || w == "any" || w == "dynamic":
			return famAny, w
		}
	}
	base := typeBase(plainType(t))
	for _, skip := range []string{"const", "final", "struct", "class", "enum", "typename"} {
		if base == skip {
			base = ""
		}
	}
	if base == "" {
		// const std::string& -> the last qualified name
		for i := len(words) - 1; i >= 0; i-- {
			if w := words[i]; w != "const" && w != "std" && w != "final" && w != "struct" {
				base = w
				break
			}
		}
	}
	if base == "" {
		return famUnknown, ""
	}
	return famClass, base
}

// colonTyped are languages that write parameters as name: Type.
var colonTyped = map[string]bool{"kotlin": true, "scala": true, "swift": true, "typescript": true, "python": true, "rust": true}

// param is one declared parameter.
type param struct {
	name, label, typ   string // label: Swift's argument label ("" for _)
	optional, variadic bool
}

// paramTypes returns the type text of each parameter of name in a
// signature, or nil when the list cannot be read (a truncated signature).
func paramTypes(sig, name, lang string) []string {
	ps := paramList(sig, name, lang)
	if ps == nil {
		return nil
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.typ
	}
	return out
}

// paramsFor returns c's parameters, from its whole parameter list when
// the signature is truncated.
func paramsFor(c Symbol) []param {
	if c.ParamList != "" {
		return paramList(c.Name+c.ParamList, c.Name, c.Lang)
	}
	return paramList(c.Signature, c.Name, c.Lang)
}

// paramList parses the parameters of name in a signature, or nil when the
// list cannot be read.
func paramList(sig, name, lang string) []param {
	rest, ok := afterName(sig, name)
	if !ok {
		return nil
	}
	items, ok := splitParams(rest)
	if !ok {
		return nil
	}
	out := make([]param, 0, len(items))
	for _, raw := range items {
		raw = strings.TrimSpace(raw)
		if raw == "" || raw == "void" && len(items) == 1 {
			continue
		}
		var p param
		text := stripDefault(raw)
		p.optional = text != raw || strings.Contains(raw, "?:")
		p.variadic = strings.Contains(text, "...") || strings.HasPrefix(text, "vararg ") || strings.HasPrefix(text, "params ") ||
			strings.Contains(text, " vararg ") || strings.HasSuffix(strings.TrimSpace(text), "*") && colonTyped[lang]
		text = stripAnnotations(text)
		if colonTyped[lang] {
			head, typ, found := strings.Cut(text, ":")
			if found {
				p.typ = strings.TrimSpace(typ)
			}
			words := strings.Fields(strings.TrimSuffix(strings.TrimSpace(head), "?"))
			if len(words) > 0 {
				p.name = words[len(words)-1]
				if lang == "swift" {
					p.label = words[0]
					if p.label == "_" {
						p.label = ""
					}
				}
			}
			out = append(out, p)
			continue
		}
		// C-family: the type is everything but the parameter name.
		fields := strings.Fields(text)
		switch len(fields) {
		case 0:
		case 1:
			p.typ = fields[0] // a prototype without names
		default:
			last := fields[len(fields)-1]
			typ := strings.TrimSpace(strings.TrimSuffix(text, last))
			// int *p, char* s[]: pointer marks stuck to the name belong to the type
			for strings.HasPrefix(last, "*") || strings.HasPrefix(last, "&") {
				typ += last[:1]
				last = last[1:]
			}
			if strings.HasSuffix(last, "[]") {
				typ += "[]"
				last = strings.TrimSuffix(last, "[]")
			}
			p.typ, p.name = typ, last
		}
		out = append(out, p)
	}
	return out
}

// splitParams splits the parenthesized list at the start of s at
// top-level commas.
func splitParams(s string) ([]string, bool) {
	if !strings.HasPrefix(s, "(") {
		return nil, false
	}
	var items []string
	depth, start := 0, 1
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '(', '[', '{', '<':
			depth++
		case ')', ']', '}', '>':
			if c == '>' && i > 0 && (s[i-1] == '-' || s[i-1] == '=') {
				continue
			}
			depth--
			if depth == 0 {
				if c != ')' {
					return nil, false
				}
				return append(items, s[start:i]), true
			}
		case ',':
			if depth == 1 {
				items = append(items, s[start:i])
				start = i + 1
			}
		case '"', '\'':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				return nil, false
			}
			i += j + 1
		}
	}
	return nil, false // unbalanced or truncated
}

func stripDefault(p string) string {
	depth := 0
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '(', '[', '{', '<':
			depth++
		case ')', ']', '}', '>':
			if c == '>' && i > 0 && (p[i-1] == '-' || p[i-1] == '=') {
				continue
			}
			depth--
		case '=':
			if depth == 0 && (i+1 >= len(p) || p[i+1] != '=' && p[i+1] != '>') && (i == 0 || p[i-1] != '=' && p[i-1] != '!') {
				return strings.TrimSpace(p[:i])
			}
		}
	}
	return p
}

// stripAnnotations drops @Annotations (with arguments) and parameter
// modifiers that are not types.
func stripAnnotations(p string) string {
	for changed := true; changed; {
		changed = false
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "@") {
			i := 1
			for i < len(p) && (isIdentRune(rune(p[i])) || p[i] == '.') {
				i++
			}
			if i < len(p) && p[i] == '(' {
				depth := 0
				for ; i < len(p); i++ {
					if p[i] == '(' {
						depth++
					} else if p[i] == ')' {
						if depth--; depth == 0 {
							i++
							break
						}
					}
				}
			}
			p, changed = p[min(i, len(p)):], true
			continue
		}
		for _, m := range []string{"final ", "vararg ", "params ", "ref ", "out ", "in ", "this ", "noinline ", "crossinline "} {
			if strings.HasPrefix(p, m) {
				p, changed = p[len(m):], true
				break
			}
		}
	}
	return p
}

func itoaInt(n int) string { return strconv.Itoa(n) }

// afterName returns the signature from the parameter list of name, past
// type parameters written after the name (Send<TResponse>(...)).
func afterName(sig, name string) (string, bool) {
	for from := 0; ; {
		at := strings.Index(sig[from:], name)
		if at < 0 {
			return "", false
		}
		at += from
		rest := strings.TrimLeft(sig[at+len(name):], " ")
		if at > 0 && isIdentRune(rune(sig[at-1])) {
			from = at + len(name)
			continue
		}
		if strings.HasPrefix(rest, "<") {
			depth := 0
			for i := 0; i < len(rest); i++ {
				if rest[i] == '<' {
					depth++
				} else if rest[i] == '>' {
					if depth--; depth == 0 {
						rest = strings.TrimLeft(rest[i+1:], " ")
						break
					}
				}
			}
		}
		if strings.HasPrefix(rest, "(") {
			return rest, true
		}
		from = at + len(name)
	}
}

// lambdaReceiverMembers returns the members named name of the implicit
// receiver of the lambdas around a reference: the receiver type of the
// function-typed parameter (R.() -> Unit) of the call each lambda is
// passed to, innermost first.
func (v *View) lambdaReceiverMembers(seg, call int, name string) []Symbol {
	sn := v.sn.Segment(seg)
	for depth := 0; call >= 0 && depth < maxTypeDepth+1; depth++ {
		targets, _ := v.resolveRef(seg, call)
		for _, t := range targets {
			if recv := lambdaReceiver(t); recv != "" {
				if ms := v.methodsOf(recv, name, 0); len(ms) > 0 {
					return ms
				}
			}
		}
		call = sn.Ref(call).Lambda
	}
	return nil
}

// lambdaReceiver returns the receiver type R of c's last parameter of a
// function type with a receiver (R.() -> Unit, suspend R.(T) -> X), or ""
// (also when R is one of c's type parameters).
func lambdaReceiver(c Symbol) string {
	params := paramsFor(c)
	for i := len(params) - 1; i >= 0; i-- {
		t := strings.TrimSpace(params[i].typ)
		t = strings.TrimSuffix(strings.TrimPrefix(t, "("), ")?")
		t = strings.TrimSpace(strings.TrimPrefix(t, "suspend "))
		at := strings.Index(t, ".(")
		if at <= 0 || !strings.Contains(t, "->") {
			continue
		}
		recv := typeBase(plainType(t[:at]))
		if recv == "" || isTypeParam(c.Signature, c.Name, recv) {
			return ""
		}
		return recv
	}
	return ""
}

// kotlinExtensions returns the Kotlin extension functions named name that
// apply to a receiver of type typ: exact for typ or one of its workspace
// supertypes, generic for a type-parameter receiver (fun <T> T.f()).
func (v *View) kotlinExtensions(typ, name string) (exact, generic []Symbol) {
	base := typeBase(typ)
	for _, s := range v.named(name, map[string]bool{facts.KindFunc: true}) {
		if !isExtension(s) {
			continue
		}
		recv := extensionReceiver(s)
		switch {
		case recv == "":
		case isTypeParam(s.Signature, s.Name, recv):
			generic = append(generic, s)
		case recv == base || v.isSubtype(base, recv, 0):
			exact = append(exact, s)
		}
	}
	return exact, generic
}

// extensionReceiver returns the receiver type's base name of a Kotlin
// extension function (fun <T> Foo<T>.f() -> Foo).
func extensionReceiver(s Symbol) string {
	end := strings.Index(s.Signature, "."+s.Name+"(")
	if end < 0 {
		end = strings.Index(s.Signature, "."+s.Name+"<")
	}
	at := strings.Index(s.Signature, "fun ")
	if end < 0 || at < 0 || end < at {
		return ""
	}
	head := strings.TrimSpace(s.Signature[at+4 : end])
	if strings.HasPrefix(head, "<") {
		depth := 0
		for i := 0; i < len(head); i++ {
			if head[i] == '<' {
				depth++
			} else if head[i] == '>' {
				if depth--; depth == 0 {
					head = strings.TrimSpace(head[i+1:])
					break
				}
			}
		}
	}
	return typeBase(plainType(head))
}
