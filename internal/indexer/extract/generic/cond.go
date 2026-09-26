package generic

import (
	"bytes"
	"strconv"
	"strings"
)

// Conditional compilation in C-family files. inactiveLines evaluates each
// #if/#ifdef/#ifndef/#elif/#else group under a default configuration and
// returns the 1-based lines of branches that are not compiled, so that a
// declaration made once per branch (Unity's typedef ... UNITY_INT) can
// prefer the compiled one. The configuration: macros #defined earlier in
// the same file (with their values), a 64-bit LP64 platform's limits, and
// every other macro undefined. A condition that cannot be evaluated (a
// function-like macro, an unknown operator) leaves its whole group active.

// platformMacros are the limits a 64-bit LP64 platform defines in
// <limits.h> and <stdint.h>.
var platformMacros = map[string]string{
	"CHAR_BIT": "8", "SCHAR_MAX": "127", "UCHAR_MAX": "255", "SHRT_MAX": "32767", "USHRT_MAX": "65535",
	"INT_MAX": "2147483647", "UINT_MAX": "0xFFFFFFFF", "LONG_MAX": "0x7FFFFFFFFFFFFFFF",
	"ULONG_MAX": "0xFFFFFFFFFFFFFFFF", "LLONG_MAX": "0x7FFFFFFFFFFFFFFF", "ULLONG_MAX": "0xFFFFFFFFFFFFFFFF",
	"INTPTR_MAX": "0x7FFFFFFFFFFFFFFF", "UINTPTR_MAX": "0xFFFFFFFFFFFFFFFF", "SIZE_MAX": "0xFFFFFFFFFFFFFFFF",
	"INT32_MAX": "2147483647", "UINT32_MAX": "0xFFFFFFFF", "INT64_MAX": "0x7FFFFFFFFFFFFFFF",
	"UINT64_MAX": "0xFFFFFFFFFFFFFFFF", "__STDC__": "1",
}

// condGroup is one open #if group.
type condGroup struct {
	outer   bool // the enclosing code is compiled
	taken   bool // a branch of this group was already compiled
	active  bool // the current branch is compiled
	unknown bool // a condition could not be evaluated: everything stays active
}

// inactiveLines returns the set of 1-based lines inside branches that the
// default configuration does not compile (directive lines excluded).
func inactiveLines(src []byte) map[int]bool {
	macros := map[string]string{}
	var groups []condGroup
	active := func() bool {
		return len(groups) == 0 || groups[len(groups)-1].active
	}
	var out map[int]bool
	lineNo := 0
	for start := 0; start < len(src); {
		end := bytes.IndexByte(src[start:], '\n')
		if end < 0 {
			end = len(src)
		} else {
			end += start
		}
		line := src[start:end]
		lineNo++
		next := end + 1
		// join continuation lines of a directive
		for bytes.HasSuffix(bytes.TrimRight(line, "\r"), []byte{'\\'}) && next < len(src) {
			e := bytes.IndexByte(src[next:], '\n')
			if e < 0 {
				e = len(src)
			} else {
				e += next
			}
			line = append(append(bytes.TrimSuffix(bytes.TrimRight(bytes.Clone(line), "\r"), []byte{'\\'}), ' '), src[next:e]...)
			next = e + 1
			lineNo++
		}
		t := bytes.TrimLeft(line, " \t")
		if len(t) == 0 || t[0] != '#' {
			if !active() {
				if out == nil {
					out = map[int]bool{}
				}
				out[lineNo] = true
			}
			start = next
			continue
		}
		word, rest := directive(t[1:])
		expr := strings.TrimSpace(stripLineComment(string(rest)))
		switch word {
		case "define":
			if active() {
				name, value := splitDefine(expr)
				if name != "" {
					macros[name] = value
				}
			}
		case "undef":
			if active() {
				delete(macros, strings.TrimSpace(expr))
			}
		case "if", "ifdef", "ifndef":
			g := condGroup{outer: active()}
			var v, ok bool
			switch word {
			case "ifdef":
				v, ok = defined(macros, expr), true
			case "ifndef":
				v, ok = !defined(macros, expr), true
			default:
				v, ok = evalCond(expr, macros)
			}
			g.unknown = !ok
			g.active = g.outer && (v || g.unknown)
			g.taken = g.active
			groups = append(groups, g)
		case "elif", "elifdef", "elifndef", "else":
			if len(groups) == 0 {
				break
			}
			g := &groups[len(groups)-1]
			if g.unknown {
				g.active = g.outer
				break
			}
			v, ok := true, true
			switch word {
			case "elif":
				v, ok = evalCond(expr, macros)
			case "elifdef":
				v = defined(macros, expr)
			case "elifndef":
				v = !defined(macros, expr)
			}
			if !ok {
				g.unknown, g.active = true, g.outer
				break
			}
			g.active = g.outer && !g.taken && v
			g.taken = g.taken || g.active
		case "endif":
			if len(groups) > 0 {
				groups = groups[:len(groups)-1]
			}
		}
		start = next
	}
	return out
}

func stripLineComment(s string) string {
	if i := strings.Index(s, "//"); i >= 0 {
		s = s[:i]
	}
	for {
		i := strings.Index(s, "/*")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i+2:], "*/")
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + " " + s[i+2+j+2:]
	}
}

// splitDefine splits "NAME value" of an object-like #define; a
// function-like macro (NAME(x) ...) is recorded without a value.
func splitDefine(expr string) (string, string) {
	i := 0
	for i < len(expr) && (isIdentByte(expr[i])) {
		i++
	}
	name := expr[:i]
	if i < len(expr) && expr[i] == '(' {
		return name, "" // function-like: defined, value unknown
	}
	return name, strings.TrimSpace(expr[i:])
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func defined(macros map[string]string, name string) bool {
	name = strings.TrimSpace(name)
	if _, ok := macros[name]; ok {
		return true
	}
	_, ok := platformMacros[name]
	return ok
}

// evalCond evaluates an #if expression; ok is false when it cannot.
func evalCond(expr string, macros map[string]string) (bool, bool) {
	p := &condParser{macros: macros}
	if !p.tokenize(expr, 0) {
		return false, false
	}
	v, ok := p.expr(0)
	if !ok || p.pos != len(p.toks) {
		return false, false
	}
	return v != 0, true
}

type condParser struct {
	macros map[string]string
	toks   []string
	pos    int
}

// tokenize splits expr into tokens, expanding object-like macros (up to a
// depth) and defined(X); an unknown identifier is 0 as in C.
func (p *condParser) tokenize(expr string, depth int) bool {
	if depth > 8 {
		return false
	}
	for i := 0; i < len(expr); {
		c := expr[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case isIdentByte(c) && !(c >= '0' && c <= '9'):
			j := i
			for j < len(expr) && isIdentByte(expr[j]) {
				j++
			}
			id := expr[i:j]
			i = j
			if id == "defined" {
				rest := strings.TrimLeft(expr[i:], " \t")
				paren := strings.HasPrefix(rest, "(")
				if paren {
					rest = strings.TrimLeft(rest[1:], " \t")
				}
				k := 0
				for k < len(rest) && isIdentByte(rest[k]) {
					k++
				}
				name := rest[:k]
				rest = strings.TrimLeft(rest[k:], " \t")
				if paren {
					if !strings.HasPrefix(rest, ")") {
						return false
					}
					rest = rest[1:]
				}
				if defined(p.macros, name) {
					p.toks = append(p.toks, "1")
				} else {
					p.toks = append(p.toks, "0")
				}
				i = len(expr) - len(rest)
				continue
			}
			if strings.HasPrefix(strings.TrimLeft(expr[i:], " \t"), "(") {
				return false // a function-like macro call
			}
			value, ok := p.macros[id]
			if !ok {
				value, ok = platformMacros[id]
			}
			switch {
			case !ok:
				p.toks = append(p.toks, "0")
			case value == "":
				return false // defined without a usable value
			default:
				p.toks = append(p.toks, "(")
				if !p.tokenize(value, depth+1) {
					return false
				}
				p.toks = append(p.toks, ")")
			}
		case c >= '0' && c <= '9':
			j := i
			for j < len(expr) && (isIdentByte(expr[j])) {
				j++
			}
			p.toks = append(p.toks, expr[i:j])
			i = j
		default:
			two := ""
			if i+1 < len(expr) {
				two = expr[i : i+2]
			}
			switch two {
			case "==", "!=", "<=", ">=", "&&", "||", "<<", ">>":
				p.toks = append(p.toks, two)
				i += 2
				continue
			}
			if !strings.ContainsRune("()!<>+-*/%&|^~?:", rune(c)) {
				return false
			}
			p.toks = append(p.toks, string(c))
			i++
		}
	}
	return true
}

var condPrec = map[string]int{
	"||": 1, "&&": 2, "|": 3, "^": 4, "&": 5, "==": 6, "!=": 6,
	"<": 7, ">": 7, "<=": 7, ">=": 7, "<<": 8, ">>": 8, "+": 9, "-": 9, "*": 10, "/": 10, "%": 10,
}

// expr parses a binary expression with precedence climbing, and the
// conditional operator at the lowest level.
func (p *condParser) expr(minPrec int) (int64, bool) {
	lhs, ok := p.unary()
	if !ok {
		return 0, false
	}
	for p.pos < len(p.toks) {
		op := p.toks[p.pos]
		if op == "?" && minPrec == 0 {
			p.pos++
			a, ok1 := p.expr(0)
			if !ok1 || p.pos >= len(p.toks) || p.toks[p.pos] != ":" {
				return 0, false
			}
			p.pos++
			b, ok2 := p.expr(0)
			if !ok2 {
				return 0, false
			}
			if lhs != 0 {
				return a, true
			}
			return b, true
		}
		prec, isOp := condPrec[op]
		if !isOp || prec < minPrec {
			break
		}
		p.pos++
		rhs, ok := p.expr(prec + 1)
		if !ok {
			return 0, false
		}
		lhs = apply(op, lhs, rhs)
	}
	return lhs, true
}

func (p *condParser) unary() (int64, bool) {
	if p.pos >= len(p.toks) {
		return 0, false
	}
	t := p.toks[p.pos]
	p.pos++
	switch t {
	case "!":
		v, ok := p.unary()
		return b2i(v == 0), ok
	case "-":
		v, ok := p.unary()
		return -v, ok
	case "+":
		return p.unary()
	case "~":
		v, ok := p.unary()
		return ^v, ok
	case "(":
		v, ok := p.expr(0)
		if !ok || p.pos >= len(p.toks) || p.toks[p.pos] != ")" {
			return 0, false
		}
		p.pos++
		return v, true
	}
	return parseCInt(t)
}

func parseCInt(t string) (int64, bool) {
	t = strings.TrimRight(t, "uUlL")
	u, err := strconv.ParseUint(t, 0, 64)
	if err != nil {
		return 0, false
	}
	return int64(u), true
}

func apply(op string, a, b int64) int64 {
	switch op {
	case "||":
		return b2i(a != 0 || b != 0)
	case "&&":
		return b2i(a != 0 && b != 0)
	case "|":
		return a | b
	case "^":
		return a ^ b
	case "&":
		return a & b
	case "==":
		return b2i(a == b)
	case "!=":
		return b2i(a != b)
	case "<":
		return b2i(uint64(a) < uint64(b))
	case ">":
		return b2i(uint64(a) > uint64(b))
	case "<=":
		return b2i(uint64(a) <= uint64(b))
	case ">=":
		return b2i(uint64(a) >= uint64(b))
	case "<<":
		return a << uint(b&63)
	case ">>":
		return a >> uint(b&63)
	case "+":
		return a + b
	case "-":
		return a - b
	case "*":
		return a * b
	case "/":
		if b == 0 {
			return 0
		}
		return a / b
	case "%":
		if b == 0 {
			return 0
		}
		return a % b
	}
	return 0
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
