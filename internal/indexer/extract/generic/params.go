package generic

import (
	"slices"
	"strings"

	gts "github.com/odvcencio/gotreesitter"

	"github.com/spawn08/chronos-code/internal/indexer/extract/packs"
	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

// arity reads the parameter list that follows the name of a function,
// method or constructor definition. Other kinds, and lists that cannot be
// read, get an unknown arity.
func (w *walker) arity(d *def, s facts.Symbol) facts.Arity {
	switch s.Kind {
	case facts.KindFunc, facts.KindMethod, facts.KindConstructor:
	default:
		return facts.Arity{}
	}
	if w.pk.Params.None {
		return facts.Arity{}
	}
	start := int(d.name.EndByte())
	end := min(len(w.src), start+maxSignatureScan)
	if start >= end {
		return facts.Arity{}
	}
	return parseArity(w.src[start:end], s.Receiver != "", w.pk.Params)
}

// paramList returns the definition's parameter list "(…)" on one line,
// for facts.Symbol.ParamList when its signature was truncated; "" when
// the list cannot be read or is longer than maxParamList.
func (w *walker) paramList(d *def) string {
	start := int(d.name.EndByte())
	end := min(len(w.src), start+maxSignatureScan)
	if start >= end {
		return ""
	}
	text := w.src[start:end]
	i := skipSpace(text, 0)
	if i < len(text) && text[i] == '<' {
		_, e, ok := group(text, i)
		if !ok {
			return ""
		}
		i = skipSpace(text, e)
	}
	if i >= len(text) || text[i] != '(' {
		return ""
	}
	_, e, ok := group(text, i)
	if !ok {
		return ""
	}
	out := collapse(string(text[i:e]))
	if len(out) > maxParamList {
		return ""
	}
	return out
}

// maxParamList bounds a recorded parameter list.
const maxParamList = 1024

// parseArity parses the parameter list at the start of text (after
// optional type parameters). method says the first parameter may be a
// receiver. A second list right after the first (Scala's curried
// parameters) makes the arity unknown.
func parseArity(text []byte, method bool, rules packs.Params) facts.Arity {
	i := skipSpace(text, 0)
	if i < len(text) && text[i] == '<' {
		_, end, ok := group(text, i)
		if !ok {
			return facts.Arity{}
		}
		i = skipSpace(text, end)
	}
	if i >= len(text) || text[i] != '(' {
		return facts.Arity{}
	}
	items, end, ok := group(text, i)
	if !ok {
		return facts.Arity{}
	}
	if j := skipSpace(text, end); j < len(text) && text[j] == '(' {
		return facts.Arity{}
	}
	a := facts.Arity{Known: true}
	for k, item := range items {
		p := strings.TrimSpace(item)
		switch {
		case p == "":
			if len(items) == 1 || k == len(items)-1 {
				continue // () or a trailing comma
			}
			return facts.Arity{}
		case p == "/": // Python's positional-only marker
			continue
		case len(items) == 1 && p == "void": // C's (void)
			continue
		case k == 0 && method && isReceiver(p, rules.Receiver):
			continue
		case rules.OptionalGroups && (p[0] == '[' || p[0] == '{'):
			inner, _, ok := group([]byte(p), 0)
			if !ok {
				return facts.Arity{}
			}
			for _, q := range inner {
				if q = strings.TrimSpace(q); q == "" {
					continue
				}
				if p[0] == '{' && slices.Contains(strings.Fields(q), "required") {
					a.Min++
				}
				a.Max++
			}
			continue
		}
		if a.Max == facts.VarArgs {
			continue // parameters after a variadic one are keyword-only
		}
		switch {
		case isVariadic(p, rules.Variadic):
			a.Max = facts.VarArgs
		case hasDefault(p):
			a.Max++
		default:
			a.Min++
			a.Max++
		}
	}
	if a.Min > facts.MaxArity || a.Max > facts.MaxArity {
		return facts.Arity{}
	}
	return a
}

// isReceiver reports whether parameter p is a method's receiver: self,
// &self, &mut self, self: Box<Self>, cls.
func isReceiver(p string, names []string) bool {
	if len(names) == 0 {
		return false
	}
	p = strings.TrimSpace(strings.TrimPrefix(p, "&"))
	if strings.HasPrefix(p, "'") { // a lifetime: &'a self
		if sp := strings.IndexByte(p, ' '); sp > 0 {
			p = strings.TrimSpace(p[sp:])
		}
	}
	p = strings.TrimSpace(strings.TrimPrefix(p, "mut "))
	name, _, _ := strings.Cut(p, ":")
	return slices.Contains(names, strings.TrimSpace(name))
}

func isVariadic(p string, words []string) bool {
	if strings.Contains(p, "...") || p[0] == '*' {
		return true
	}
	for _, f := range strings.Fields(p) {
		if slices.Contains(words, f) {
			return true
		}
	}
	return false
}

// hasDefault reports a default value (a top-level "=") or an optional
// marker ("a?: T").
func hasDefault(p string) bool {
	depth := 0
	for j := 0; j < len(p); j++ {
		switch c := p[j]; c {
		case '(', '[', '{', '<':
			depth++
		case ')', ']', '}':
			depth--
		case '>':
			if j > 0 && (p[j-1] == '-' || p[j-1] == '=') {
				continue
			}
			depth--
		case '?':
			if depth == 0 && j+1 < len(p) && p[j+1] == ':' {
				return true
			}
		case '=':
			if depth != 0 {
				continue
			}
			next := byte(0)
			if j+1 < len(p) {
				next = p[j+1]
			}
			prev := byte(0)
			if j > 0 {
				prev = p[j-1]
			}
			if next != '=' && next != '>' && prev != '=' && prev != '!' && prev != '<' && prev != '>' {
				return true
			}
		}
	}
	return false
}

func skipSpace(text []byte, i int) int {
	for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r') {
		i++
	}
	return i
}

// group scans the bracketed group opening at text[i] ("(", "[", "{" or
// "<") and returns its items, split at top-level commas, and the index
// after its closing bracket. Strings, comments, "->" and "=>" are skipped;
// a ">" that closes nothing is a comparison. ok is false for an
// unbalanced group.
func group(text []byte, i int) (items []string, end int, ok bool) {
	stack := []byte{closer(text[i])}
	start := i + 1
	for j := i + 1; j < len(text); j++ {
		switch c := text[j]; {
		case c == '"' || c == '`' || c == '\'' && prevNonSpace(text, j) == '=':
			k := closeQuote(text, j)
			if k < 0 {
				return nil, 0, false
			}
			j = k
		case c == '/' && j+1 < len(text) && text[j+1] == '/',
			c == '#' && (j+1 >= len(text) || text[j+1] != '['):
			for j < len(text) && text[j] != '\n' {
				j++
			}
		case c == '/' && j+1 < len(text) && text[j+1] == '*':
			k := strings.Index(string(text[j+2:]), "*/")
			if k < 0 {
				return nil, 0, false
			}
			j += k + 3
		case (c == '-' || c == '=') && j+1 < len(text) && text[j+1] == '>':
			j++
		case c == '(' || c == '[' || c == '{' || c == '<':
			stack = append(stack, closer(c))
		case c == ')' || c == ']' || c == '}' || c == '>':
			if c != stack[len(stack)-1] {
				if c == '>' {
					continue
				}
				return nil, 0, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return append(items, string(text[start:j])), j + 1, true
			}
		case c == ',' && len(stack) == 1:
			items = append(items, string(text[start:j]))
			start = j + 1
		}
	}
	return nil, 0, false
}

func closer(open byte) byte {
	switch open {
	case '(':
		return ')'
	case '[':
		return ']'
	case '{':
		return '}'
	}
	return '>'
}

func prevNonSpace(text []byte, j int) byte {
	for j--; j >= 0; j-- {
		if c := text[j]; c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return c
		}
	}
	return 0
}

// closeQuote returns the index of the quote closing the string opened at
// text[j], or -1.
func closeQuote(text []byte, j int) int {
	q := text[j]
	for k := j + 1; k < len(text); k++ {
		switch text[k] {
		case '\\':
			k++
		case q:
			return k
		}
	}
	return -1
}

// callArgs returns facts.Ref.Args and facts.Ref.ArgTypes for the call or
// instantiation node n by the pack's call rule: 1 + the argument count
// (0 when the pack has no rule, no argument list is found or it contains
// a syntax error), and a type hint per argument (see argHint).
func (w *walker) callArgs(n *gts.Node) (uint8, string) {
	args, types, _ := w.callArgsLambdas(n)
	return args, types
}

// callArgsLambdas is callArgs that also returns the call's lambda
// arguments (trailing or in the list), for facts.Ref.Lambda.
func (w *walker) callArgsLambdas(n *gts.Node) (uint8, string, []*gts.Node) {
	rule := w.pk.Calls
	if n == nil || len(rule.Arguments) == 0 {
		return 0, "", nil
	}
	var lambdas []*gts.Node
	var hints []string
	found, broken := false, false
	var visit func(p *gts.Node, deep bool)
	visit = func(p *gts.Node, deep bool) {
		for i := 0; i < p.ChildCount(); i++ {
			ch := p.Child(i)
			switch t := ch.Type(w.lang); {
			case slices.Contains(rule.Arguments, t):
				found = true
				broken = broken || ch.HasError()
				for k := 0; k < ch.NamedChildCount(); k++ {
					if a := ch.NamedChild(k); !isComment(a.Type(w.lang)) {
						h := w.argHint(a)
						hints = append(hints, h)
						if strings.HasSuffix(h, "#f") {
							lambdas = append(lambdas, a)
						}
					}
				}
			case slices.Contains(rule.Trailing, t):
				found = true
				hints = append(hints, "#t") // a trailing lambda or block
				lambdas = append(lambdas, ch)
			case deep && slices.Contains(rule.Through, t):
				visit(ch, false)
			}
		}
	}
	visit(n, true)
	// Swift parses f(a) { } as a call of the call f(a) with a trailing
	// closure: the outer call's closures are f's arguments too, but an
	// outer argument list (f(a)(b)) calls f's result.
	for p := n.Parent(); found && p != nil && p.Type(w.lang) == n.Type(w.lang) && p.ChildCount() > 0 && key(p.Child(0)) == key(n); p = p.Parent() {
		extra, lists := w.trailingOnly(p)
		if lists {
			break
		}
		for ; extra > 0; extra-- {
			hints = append(hints, "#t")
		}
		for i := 1; i < p.ChildCount(); i++ {
			lambdas = append(lambdas, p.Child(i)) // the closures, inside call suffixes
		}
		n = p
	}
	if !found || broken || len(hints) > facts.MaxArity {
		return 0, "", lambdas
	}
	types := strings.Join(hints, ",")
	if strings.Trim(types, ",") == "" || len(types) > maxArgTypes {
		types = ""
	}
	return uint8(len(hints) + 1), types, lambdas
}

// maxArgTypes bounds the recorded argument type hints of one call.
const maxArgTypes = 256

// argHint classifies one argument for overload selection (facts.Ref
// ArgTypes): a literal's class (#s string, #n number, #b boolean, #c
// character, #0 null), #f for a lambda or function reference, an
// identifier's name (typed at query time from binding hints and enum
// members), @T for a constructed T, ()f for the result of a call to f, or
// "". A named argument (Kotlin a = 1, C# a: 1, Swift and Dart labels) is
// prefixed with its name: "a=#n".
func (w *walker) argHint(a *gts.Node) string {
	name := ""
	// Named and labelled argument wrappers (C# argument, Kotlin and Swift
	// value_argument, Dart named_argument): the value is the last child.
	for depth := 0; depth < 2 && strings.Contains(a.Type(w.lang), "argument") && a.NamedChildCount() > 0; depth++ {
		if name == "" && a.NamedChildCount() >= 2 {
			name = w.argName(a)
		}
		a = a.NamedChild(a.NamedChildCount() - 1)
	}
	h := w.valueHint(a)
	if name != "" && !strings.ContainsAny(name, ",=@#() ") {
		return name + "=" + h
	}
	return h
}

// argName returns the name of a named argument wrapper: a label child
// (Swift, Dart), or an identifier followed by "=" or ":" (Kotlin, C#).
func (w *walker) argName(a *gts.Node) string {
	first := a.NamedChild(0)
	if strings.Contains(first.Type(w.lang), "label") || strings.Contains(first.Type(w.lang), "name_colon") {
		return strings.TrimSuffix(strings.TrimSpace(w.text(first)), ":")
	}
	if t := first.Type(w.lang); t != "identifier" && t != "simple_identifier" {
		return ""
	}
	for i := 0; i < a.ChildCount(); i++ {
		if c := a.Child(i); !c.IsNamed() && (w.text(c) == "=" || w.text(c) == ":") {
			return w.text(first)
		}
	}
	return ""
}

var lambdaTypes = []string{"lambda", "closure", "function_expression", "anonymous_function", "arrow_function",
	"function_literal", "method_reference", "callable_reference", "anonymous_method"}

func (w *walker) valueHint(a *gts.Node) string {
	t := a.Type(w.lang)
	if strings.Contains(t, "reference") || strings.Contains(t, "class_literal") {
		// Foo::class, Foo.class: a class literal; Foo::bar: a function
		if text := w.text(a); strings.HasSuffix(text, "::class") {
			return "@KClass"
		} else if strings.HasSuffix(text, ".class") {
			return "@Class"
		}
	}
	for _, l := range lambdaTypes {
		if strings.Contains(t, l) {
			return "#f"
		}
	}
	switch {
	case strings.Contains(t, "char") && strings.Contains(t, "literal"):
		return "#c"
	case strings.Contains(t, "string"):
		return "#s"
	case t == "true" || t == "false" || strings.Contains(t, "boolean") || t == "bool_literal":
		return "#b"
	case t == "null" || t == "null_literal" || t == "nullptr" || t == "nil" || t == "none":
		return "#0"
	case strings.Contains(t, "integer") || strings.Contains(t, "decimal") || strings.Contains(t, "float") ||
		strings.Contains(t, "number") || strings.Contains(t, "real_literal") || strings.Contains(t, "hex"):
		return "#n"
	case t == "identifier" || t == "simple_identifier":
		name := w.text(a)
		switch name {
		case "true", "false":
			return "#b"
		case "null", "nil", "None", "NULL", "nullptr":
			return "#0"
		}
		if strings.ContainsAny(name, ",=@#() ") {
			return ""
		}
		return name
	case t == "object_creation_expression" || t == "new_expression" || t == "instance_expression":
		typ := a.ChildByFieldName("type", w.lang)
		if typ == nil {
			for k := 0; k < a.NamedChildCount(); k++ {
				if c := a.NamedChild(k); strings.Contains(c.Type(w.lang), "type") || strings.Contains(c.Type(w.lang), "identifier") {
					typ = c
					break
				}
			}
		}
		if typ == nil {
			return ""
		}
		if name := normalizeType(w.text(typ)); name != "" && !strings.ContainsAny(name, ",=@#() ") {
			return "@" + name
		}
	case t == "call_expression" || t == "method_invocation" || t == "invocation_expression":
		fn := a.ChildByFieldName("name", w.lang) // Java method_invocation
		if fn == nil {
			fn = a.ChildByFieldName("function", w.lang)
		}
		if fn == nil && a.NamedChildCount() > 0 {
			fn = a.NamedChild(0) // Kotlin, Swift
		}
		if fn == nil {
			return ""
		}
		callee, _ := splitQualified(collapse(w.text(fn)))
		if callee == "" || !isIdent(callee) {
			return ""
		}
		return "()" + callee
	}
	return ""
}

func isIdent(s string) bool {
	for _, r := range s {
		if !(r == '_' || r == '$' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return s != ""
}

// trailingOnly counts the trailing arguments among the children of call
// node p (and of its Through children); lists reports an argument list.
func (w *walker) trailingOnly(p *gts.Node) (n int, lists bool) {
	rule := w.pk.Calls
	for i := 1; i < p.ChildCount(); i++ {
		ch := p.Child(i)
		kids := []*gts.Node{ch}
		if slices.Contains(rule.Through, ch.Type(w.lang)) {
			kids = kids[:0]
			for k := 0; k < ch.ChildCount(); k++ {
				kids = append(kids, ch.Child(k))
			}
		}
		for _, c := range kids {
			switch t := c.Type(w.lang); {
			case slices.Contains(rule.Arguments, t):
				return n, true
			case slices.Contains(rule.Trailing, t):
				n++
			}
		}
	}
	return n, false
}
