package main

import "strings"

// scip-clang (C and C++) records some references to functions and methods
// that no reference in source text can match. clangNotReference names the
// reason, or returns "" for a reference to score:
//
//   - "not the callable's name": the occurrence's text differs from the
//     function's name. scip-clang puts implicit constructor calls on the
//     variable (XMLDocument doc(true);) and operator calls on the operator.
//   - "forward declaration": the occurrence is the name in a prototype or in
//     a member declaration of a class body (defined out of line).
//     scip-clang records forward declarations as references by design (its
//     docs/Design.md, "Forward declarations"), and the declaration may be
//     one the indexer does not parse (CJSON_PUBLIC(void) cJSON_Delete(...);),
//     so it is recognized from the source text.
func clangNotReference(symbol, text string, lines []string, line0, start, end int) string {
	name, owner := callableName(symbol)
	if text != name && "~"+text != name {
		return "not the callable's name"
	}
	ctor := strings.TrimPrefix(name, "~") == owner
	if forwardDecl(lines, line0, start, end, ctor) {
		return "forward declaration"
	}
	return ""
}

// symbolName returns the name of a SCIP method or type descriptor; for a
// constructor (<init>, .ctor) the name of its class.
func symbolName(symbol string) string {
	if strings.HasSuffix(symbol, ").") {
		name, owner := callableName(symbol)
		if name == "<init>" || name == ".ctor" {
			return owner
		}
		return name
	}
	name, _ := callableName(strings.TrimSuffix(symbol, "#") + "().")
	return name
}

// callableName returns the name of a SCIP method descriptor, name(disambiguator).,
// and of the descriptor before it (the class of a method).
func callableName(symbol string) (name, owner string) {
	s := strings.TrimSuffix(symbol, ").")
	if i := strings.LastIndexByte(s, '('); i >= 0 {
		s = s[:i]
	}
	last := func(s string) (string, string) {
		if strings.HasSuffix(s, "`") {
			if j := strings.LastIndexByte(s[:len(s)-1], '`'); j >= 0 {
				return s[j+1 : len(s)-1], s[:max(j-1, 0)]
			}
		}
		j := strings.LastIndexAny(s, " /#.")
		return s[j+1:], s[:max(j, 0)]
	}
	name, rest := last(s)
	owner, _ = last(rest)
	return name, owner
}

// statementKey holds the keywords that start a statement or an expression
// rather than a declaration.
var statementKey = map[string]bool{
	"return": true, "else": true, "case": true, "default": true, "do": true, "if": true,
	"while": true, "for": true, "switch": true, "goto": true, "throw": true, "delete": true,
	"new": true, "sizeof": true, "co_return": true, "co_await": true, "co_yield": true,
}

// forwardDecl reports whether the name at lines[line0][start:end] is
// declared there without a body: it is followed by a parameter list, any
// trailing qualifiers and ";", and preceded in its statement only by
// declaration specifiers (a return type, storage classes, macros such as
// CJSON_PUBLIC(T), templates), which a constructor or destructor may lack.
func forwardDecl(lines []string, line0, start, end int, ctor bool) bool {
	after := stripComments(window(lines, line0, end, 30, true))
	rest, ok := skipParens(strings.TrimLeft(after, " \t\n"))
	if !ok {
		return false
	}
	for {
		rest = strings.TrimLeft(rest, " \t\n")
		switch {
		case strings.HasPrefix(rest, ";"):
			return declPrefix(lines, line0, start, ctor)
		case strings.HasPrefix(rest, "&"):
			rest = rest[1:]
		case strings.HasPrefix(rest, "="):
			rest = strings.TrimLeft(rest[1:], " \t\n")
			w := identPrefix(rest)
			if w != "0" && w != "default" && w != "delete" {
				return false
			}
			rest = rest[len(w):]
		default:
			w := identPrefix(rest)
			if w == "" { // not const, override, noexcept(...), a macro, ...
				return false
			}
			rest = strings.TrimLeft(rest[len(w):], " \t\n")
			if strings.HasPrefix(rest, "(") {
				if rest, ok = skipParens(rest); !ok {
					return false
				}
			}
		}
	}
}

// declPrefix reports whether the statement text before lines[line0][:start]
// consists of declaration specifiers.
func declPrefix(lines []string, line0, start int, ctor bool) bool {
	before := stripComments(window(lines, line0, start, 10, false))
	if i := strings.LastIndex(before, "*/"); i >= 0 { // a comment opened before the window
		before = before[i+2:]
	}
	if i := strings.LastIndexAny(before, ";{}"); i >= 0 {
		before = before[i+1:]
	}
	p := strings.TrimSpace(before)
	for _, spec := range []string{"public", "protected", "private"} { // an access label
		if rest := strings.TrimSpace(strings.TrimPrefix(p, spec)); rest != p && strings.HasPrefix(rest, ":") && !strings.HasPrefix(rest, "::") {
			p = strings.TrimSpace(rest[1:])
		}
	}
	p = strings.TrimSpace(strings.TrimSuffix(p, "~"))
	// Template arguments may hold anything; drop them.
	for {
		i := strings.LastIndexByte(p, '<')
		if i < 0 {
			break
		}
		j := strings.IndexByte(p[i:], '>')
		if j < 0 {
			return false
		}
		p = p[:i] + " " + p[i+j+1:]
	}
	if p == "" {
		return ctor
	}
	if first := identPrefix(p); first == "" && p[0] != '[' || statementKey[first] {
		return false
	}
	p = strings.ReplaceAll(p, "::", " ")
	prev, depth := "", 0 // identifier before the current position; parentheses
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case depth > 0: // a macro's arguments: CJSON_PUBLIC(cJSON *)
			switch c {
			case '(':
				depth++
			case ')':
				depth--
			}
		case isIdentByte(c):
			w := identPrefix(p[i:])
			prev = w
			i += len(w) - 1
			continue
		case c == ' ' || c == '\t' || c == '\n':
			continue
		case c == '(':
			if prev == "" || statementKey[prev] { // a cast or a condition
				return false
			}
			depth++
		case c == '*' || c == '&' || c == '[' || c == ']':
		default: // =, ., ->, :, operators, literals: an expression
			return false
		}
		prev = ""
	}
	last := p[len(p)-1]
	return depth == 0 && isIdentByte(last) || last == '*' || last == '&' || last == ')' || last == ']'
}

// window returns the text after (forward) or before column col of
// lines[line0], extended over at most n further or previous lines and
// joined with newlines. Preprocessor lines are left out.
func window(lines []string, line0, col, n int, forward bool) string {
	var parts []string
	if forward {
		parts = append(parts, lines[line0][col:])
		for l := line0 + 1; l < len(lines) && l <= line0+n; l++ {
			if !strings.HasPrefix(strings.TrimSpace(lines[l]), "#") {
				parts = append(parts, lines[l])
			}
		}
		return strings.Join(parts, "\n")
	}
	parts = append(parts, lines[line0][:col])
	for l := line0 - 1; l >= 0 && l >= line0-n; l-- {
		if t := strings.TrimSpace(lines[l]); strings.HasPrefix(t, "#") {
			break
		}
		parts = append([]string{lines[l]}, parts...)
	}
	return strings.Join(parts, "\n")
}

// stripComments blanks C comments and the contents of string and character
// literals (leaving empty quotes), so that punctuation in them is ignored.
func stripComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], "//"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				return b.String()
			}
			i += j - 1
		case strings.HasPrefix(s[i:], "/*"):
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return b.String()
			}
			b.WriteByte(' ')
			i += j + 3
		case s[i] == '"' || s[i] == '\'':
			q := s[i]
			j := i + 1
			for j < len(s) && s[j] != q && s[j] != '\n' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			b.WriteByte(q)
			b.WriteByte(q)
			i = j
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// skipParens skips a balanced parenthesized group at the start of s.
func skipParens(s string) (string, bool) {
	if !strings.HasPrefix(s, "(") {
		return s, false
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[i+1:], true
			}
		}
	}
	return s, false
}

func identPrefix(s string) string {
	i := 0
	for i < len(s) && isIdentByte(s[i]) {
		i++
	}
	return s[:i]
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
