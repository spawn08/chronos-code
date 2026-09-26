package scip

import "strings"

// Descriptor suffixes (scip.proto, Descriptor.Suffix).
const (
	SuffixNamespace     = '/'
	SuffixType          = '#'
	SuffixTerm          = '.'
	SuffixMeta          = ':'
	SuffixMacro         = '!'
	SuffixMethod        = 'm' // name(disambiguator).
	SuffixTypeParameter = '['
	SuffixParameter     = '('
)

// Descriptor is one descriptor of a symbol.
type Descriptor struct {
	Name   string
	Suffix byte
}

// Symbol is a parsed SCIP symbol. Local symbols ("local 12") have Local
// set and nothing else.
type Symbol struct {
	Local       bool
	Scheme      string
	Package     string // manager, name and version, space-separated
	Descriptors []Descriptor
}

// ParseSymbol parses a symbol in the SCIP symbol grammar. ok is false for
// a malformed one.
func ParseSymbol(s string) (sym Symbol, ok bool) {
	if strings.HasPrefix(s, "local ") {
		return Symbol{Local: true}, true
	}
	var fields [4]string
	rest := s
	for i := range fields {
		f, r, ok := spaceField(rest)
		if !ok {
			return Symbol{}, false
		}
		fields[i], rest = f, r
	}
	sym.Scheme = fields[0]
	sym.Package = fields[1] + " " + fields[2] + " " + fields[3]
	for rest != "" {
		d, r, ok := descriptor(rest)
		if !ok {
			return Symbol{}, false
		}
		sym.Descriptors = append(sym.Descriptors, d)
		rest = r
	}
	return sym, len(sym.Descriptors) > 0
}

// spaceField reads one space-terminated field in which "  " stands for a
// space.
func spaceField(s string) (field, rest string, ok bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == ' ' {
			b.WriteByte(' ')
			i++
			continue
		}
		return b.String(), s[i+1:], true
	}
	return "", "", false
}

func descriptor(s string) (Descriptor, string, bool) {
	switch s[0] {
	case '[':
		name, rest, ok := identifier(s[1:])
		if !ok || !strings.HasPrefix(rest, "]") {
			return Descriptor{}, "", false
		}
		return Descriptor{name, SuffixTypeParameter}, rest[1:], true
	case '(':
		name, rest, ok := identifier(s[1:])
		if !ok || !strings.HasPrefix(rest, ")") {
			return Descriptor{}, "", false
		}
		return Descriptor{name, SuffixParameter}, rest[1:], true
	}
	name, rest, ok := identifier(s)
	if !ok || rest == "" {
		return Descriptor{}, "", false
	}
	switch rest[0] {
	case '/', '#', '.', ':', '!':
		return Descriptor{name, rest[0]}, rest[1:], true
	case '(':
		end := strings.Index(rest, ").")
		if end < 0 {
			return Descriptor{}, "", false
		}
		return Descriptor{name, SuffixMethod}, rest[end+2:], true
	}
	return Descriptor{}, "", false
}

// identifier reads a simple identifier ([_+\-$a-zA-Z0-9]+) or a
// backtick-escaped one (“ stands for a backtick).
func identifier(s string) (string, string, bool) {
	if strings.HasPrefix(s, "`") {
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] != '`' {
				b.WriteByte(s[i])
				continue
			}
			if i+1 < len(s) && s[i+1] == '`' {
				b.WriteByte('`')
				i++
				continue
			}
			return b.String(), s[i+1:], b.Len() > 0
		}
		return "", "", false
	}
	i := 0
	for i < len(s) && simpleChar(s[i]) {
		i++
	}
	return s[:i], s[i:], i > 0
}

func simpleChar(c byte) bool {
	return c == '_' || c == '+' || c == '-' || c == '$' ||
		'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// Last returns the symbol's last descriptor.
func (s Symbol) Last() Descriptor {
	if len(s.Descriptors) == 0 {
		return Descriptor{}
	}
	return s.Descriptors[len(s.Descriptors)-1]
}

// Owner returns the name of the descriptor before the last one (a
// method's class), or "".
func (s Symbol) Owner() string {
	if len(s.Descriptors) < 2 {
		return ""
	}
	return s.Descriptors[len(s.Descriptors)-2].Name
}

// Callable reports whether the symbol names a method or function.
func (s Symbol) Callable() bool { return s.Last().Suffix == SuffixMethod }

// IsType reports whether the symbol names a type.
func (s Symbol) IsType() bool { return s.Last().Suffix == SuffixType }

// constructorNames are method names indexers give constructors; a
// reference to one is spelled with the class name (new Foo()).
var constructorNames = map[string]bool{"<init>": true, ".ctor": true, "<constructor>": true, "constructor": true, "__init__": true}

// Constructor reports whether s names a constructor (<init>, .ctor, or a
// method named like its class, as scip-clang names them). Indexers also
// record implicit constructor calls at places that do not spell the class
// (C++ `T x;` at x, C# attributes without their Attribute suffix, Java
// enum constants, this(...) and super(...)).
func (s Symbol) Constructor() bool {
	if !s.Callable() {
		return false
	}
	name := s.Last().Name
	return constructorNames[name] || name == s.Owner()
}

// operatorNames are methods Kotlin and Scala call through syntax: f() for
// invoke or apply, a[i] for get, for loops for iterator, destructuring for
// componentN, operators for plus, not, compareTo and so on.
var operatorNames = map[string]bool{
	"invoke": true, "apply": true, "get": true, "set": true, "iterator": true, "hasNext": true, "next": true,
	"contains": true, "not": true, "plus": true, "minus": true, "times": true, "div": true, "rem": true,
	"rangeTo": true, "rangeUntil": true, "compareTo": true, "equals": true, "unaryMinus": true, "unaryPlus": true,
	"inc": true, "dec": true, "getValue": true, "setValue": true, "provideDelegate": true,
	"plusAssign": true, "minusAssign": true,
}

// Implicit reports whether references to s may be spelled by syntax
// rather than its name: constructors and operator conventions.
func (s Symbol) Implicit() bool {
	if s.Constructor() {
		return true
	}
	name := s.Last().Name
	return s.Callable() && (operatorNames[name] || strings.HasPrefix(name, "component") && isDigits(name[len("component"):]))
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// Spellings returns the texts a reference to s can have in source: its
// last descriptor's name; for a constructor, its class's name; for a
// get/set accessor, the property's name (Kotlin, through scip-java). It
// returns nil when the name is not an identifier (operators, destructors,
// synthetic names such as $anonymous_type_0), which cannot be checked
// against source text.
func (s Symbol) Spellings() []string {
	last := s.Last()
	if strings.HasPrefix(last.Name, "$") {
		return nil
	}
	if s.Callable() {
		for _, prefix := range []string{"get", "set"} {
			if rest, ok := strings.CutPrefix(last.Name, prefix); ok && rest != "" && rest[0] >= 'A' && rest[0] <= 'Z' {
				// Defined at the property, or at its get()/set() body.
				return []string{last.Name, rest, strings.ToLower(rest[:1]) + rest[1:], prefix}
			}
		}
	}
	if s.Callable() && constructorNames[last.Name] {
		out := []string{}
		if owner := s.Owner(); isIdent(owner) {
			out = append(out, owner)
		}
		if isIdent(last.Name) {
			out = append(out, last.Name)
		}
		if last.Name != "constructor" {
			// TypeScript and Kotlin constructors are defined at the keyword.
			out = append(out, "constructor")
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	if !isIdent(last.Name) {
		return nil
	}
	return []string{last.Name}
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || r == '$':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r > 0x7f:
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
