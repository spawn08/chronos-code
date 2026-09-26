package contracts

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos-code/indexer/facts"
)

const maxSignatureBytes = 200

// Extract fills f from a contract file: its Lang, PkgName (proto package,
// thrift namespace) and the contract nodes it declares. Unparsable input is
// noted in f.ParseErr; whatever was found is kept.
func Extract(f *facts.File, lang string, src []byte) {
	f.Lang = lang
	switch lang {
	case LangProto:
		extractIDL(f, src, protoSyntax)
	case LangThrift:
		extractIDL(f, src, thriftSyntax)
	case LangGraphQL:
		extractGraphQL(f, src)
	case LangSQL:
		extractSQL(f, src)
	case LangOpenAPI:
		if err := extractOpenAPI(f, src); err != nil {
			f.ParseErr = err.Error()
		}
	}
}

type token struct {
	text string
	line int
	str  bool // a string literal (text is unquoted)
}

// lex splits an IDL into identifiers (dots included), string literals and
// single punctuation characters, dropping //, # and /* */ comments.
func lex(src []byte, hashComments bool) []token {
	var out []token
	line := 1
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '/', c == '#' && hashComments:
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && (src[i] != '*' || src[i+1] != '/') {
				if src[i] == '\n' {
					line++
				}
				i++
			}
			i += 2
		case c == '"' || c == '\'':
			start, l := i+1, line
			i++
			for i < len(src) && src[i] != c {
				switch src[i] {
				case '\\':
					i++
				case '\n':
					line++
				}
				i++
			}
			end := min(i, len(src))
			out = append(out, token{text: string(src[start:end]), line: l, str: true})
			i++
		case isIdent(c):
			start := i
			for i < len(src) && (isIdent(src[i]) || src[i] == '.') {
				i++
			}
			out = append(out, token{text: string(src[start:i]), line: line})
		default:
			out = append(out, token{text: string(c), line: line})
			i++
		}
	}
	return out
}

func isIdent(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// idlSyntax says how an interface definition language names its parts.
type idlSyntax struct {
	pkg      []string // statements naming the package ("package"; thrift "namespace <lang>")
	service  string
	messages []string // declarations recorded as messages
	enums    []string
	hash     bool // '#' starts a comment
	// rpcWord introduces a method ("rpc"); "" means any "name(" inside a
	// service is one (thrift).
	rpcWord string
}

var (
	protoSyntax  = idlSyntax{pkg: []string{"package"}, service: "service", messages: []string{"message"}, enums: []string{"enum"}, rpcWord: "rpc"}
	thriftSyntax = idlSyntax{pkg: []string{"namespace"}, service: "service", messages: []string{"struct", "union", "exception"}, enums: []string{"enum"}, hash: true}
)

type idlScope struct {
	kind string // "service", "message", "enum", "other"
	name string
	sym  int // index into f.Symbols, or -1
}

// extractIDL reads Protobuf and Thrift: services become namespaces holding
// one rpc node per method (key Service/Method), messages (nested ones as
// Outer.Inner), structs, unions and exceptions become message nodes.
func extractIDL(f *facts.File, src []byte, syn idlSyntax) {
	toks := lex(src, syn.hash)
	var stack []idlScope
	pending := idlScope{kind: "other", sym: -1} // the declaration the next '{' opens
	outer := func() string {
		var names []string
		for _, s := range stack {
			if s.kind == "message" {
				names = append(names, s.name)
			}
		}
		return strings.Join(names, ".")
	}
	add := func(s facts.Symbol) int {
		s.Exported, s.Visibility = true, facts.VisPublic
		if len(stack) > 0 && stack[len(stack)-1].sym >= 0 {
			s.Container = stack[len(stack)-1].sym + 1
		}
		f.Symbols = append(f.Symbols, s)
		return len(f.Symbols) - 1
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.str {
			continue
		}
		inService := len(stack) > 0 && stack[len(stack)-1].kind == "service"
		switch {
		case t.text == "{":
			stack = append(stack, pending)
			pending = idlScope{kind: "other", sym: -1}
		case t.text == "}":
			if n := len(stack); n > 0 {
				if s := stack[n-1]; s.sym >= 0 {
					f.Symbols[s.sym].EndLine = t.line
				}
				stack = stack[:n-1]
			}
		case slices.Contains(syn.pkg, t.text) && len(stack) == 0 && i+1 < len(toks):
			// thrift: namespace <lang> <name>; proto: package <name>;
			j := i + 1
			if t.text == "namespace" && j+1 < len(toks) {
				j++
			}
			if f.PkgName == "" {
				f.PkgName = toks[j].text
			}
			i = j
		case t.text == syn.service && i+1 < len(toks) && !inService && isName(toks[i+1].text):
			name := toks[i+1].text
			pending = idlScope{kind: "service", name: name}
			pending.sym = add(facts.Symbol{Name: name, Kind: facts.KindNamespace, Line: t.line, EndLine: t.line, Signature: syn.service + " " + name})
			i++
		case (slices.Contains(syn.messages, t.text) || slices.Contains(syn.enums, t.text)) && i+1 < len(toks) && !inService && isName(toks[i+1].text):
			name := toks[i+1].text
			if o := outer(); o != "" {
				name = o + "." + name
			}
			kind := facts.KindMessage
			if slices.Contains(syn.enums, t.text) {
				kind = facts.KindEnum
			}
			pending = idlScope{kind: "message", name: toks[i+1].text}
			if kind == facts.KindEnum {
				pending.kind = "enum"
			}
			pending.sym = add(facts.Symbol{Name: name, Kind: kind, Line: t.line, EndLine: t.line, Signature: t.text + " " + name})
			i++
		case inService:
			name, sigStart := "", i
			switch {
			case syn.rpcWord != "" && t.text == syn.rpcWord && i+1 < len(toks):
				name = toks[i+1].text
			case syn.rpcWord == "" && i+1 < len(toks) && toks[i+1].text == "(" && isName(t.text):
				name = t.text
				// the return type and "oneway" come before the name
				for sigStart > 0 && toks[sigStart-1].line == t.line && isName(toks[sigStart-1].text) {
					sigStart--
				}
			}
			if name == "" {
				continue
			}
			svc := stack[len(stack)-1].name
			end := i + 1
			depth := 0
			for ; end < len(toks); end++ {
				tx := toks[end].text
				switch tx {
				case "(":
					depth++
				case ")":
					depth--
				}
				if depth == 0 && (tx == ";" || tx == "{" || tx == "}" || tx == "," && syn.rpcWord == "") {
					break
				}
				if depth == 0 && syn.rpcWord == "" && end > i+1 && toks[end].line != toks[end-1].line && toks[end-1].text == ")" && toks[end].text != "throws" {
					break
				}
			}
			var sig strings.Builder
			for k := sigStart; k < min(end, len(toks)); k++ {
				if sig.Len() > 0 && toks[k].text != "(" && toks[k].text != ")" && toks[k-1].text != "(" {
					sig.WriteByte(' ')
				}
				sig.WriteString(toks[k].text)
			}
			add(facts.Symbol{Name: RPCKey(svc, name), Kind: facts.KindRPC, Line: t.line, EndLine: t.line, Signature: clip(sig.String())})
			i = max(i, end-1)
			if end < len(toks) && toks[end].text == "{" {
				i = end - 1 // the '{' of rpc options opens a scope
			}
		}
	}
}

func isName(s string) bool {
	if s == "" || !isIdent(s[0]) || s[0] >= '0' && s[0] <= '9' {
		return false
	}
	switch s {
	case "throws", "oneway", "extends", "returns", "stream":
		return false
	}
	return true
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxSignatureBytes {
		s = s[:maxSignatureBytes-3] + "..."
	}
	return s
}

var graphqlRoots = map[string]bool{"Query": true, "Mutation": true, "Subscription": true}

// extractGraphQL reads a schema: the fields of Query, Mutation and
// Subscription become rpc nodes keyed "Query/Field"; other types, inputs,
// interfaces, enums, unions and scalars become message nodes.
func extractGraphQL(f *facts.File, src []byte) {
	toks := lex(src, true)
	depth, root, rootSym := 0, "", -1
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.str {
			continue
		}
		switch t.text {
		case "{":
			depth++
			continue
		case "}":
			depth--
			if depth == 0 && rootSym >= 0 {
				f.Symbols[rootSym].EndLine = t.line
				root, rootSym = "", -1
			}
			continue
		case "(":
			// skip argument lists, which hold names and colons too
			for d := 0; i < len(toks); i++ {
				if toks[i].text == "(" {
					d++
				} else if toks[i].text == ")" {
					if d--; d == 0 {
						break
					}
				}
			}
			continue
		}
		if depth == 0 {
			switch t.text {
			case "type", "input", "interface", "enum", "union", "scalar":
				if i+1 >= len(toks) {
					continue
				}
				name := toks[i+1].text
				f.Symbols = append(f.Symbols, facts.Symbol{Name: name, Kind: facts.KindMessage, Line: t.line, EndLine: t.line,
					Signature: t.text + " " + name, Exported: true, Visibility: facts.VisPublic})
				if graphqlRoots[name] {
					root, rootSym = name, len(f.Symbols)-1
				}
				i++
			}
			continue
		}
		if depth == 1 && root != "" && isName(t.text) && i+1 < len(toks) && (toks[i+1].text == ":" || toks[i+1].text == "(") {
			f.Symbols = append(f.Symbols, facts.Symbol{Name: RPCKey(root, t.text), Kind: facts.KindRPC, Line: t.line, EndLine: t.line,
				Signature: clip(graphqlField(toks, i)), Exported: true, Visibility: facts.VisPublic, Container: rootSym + 1})
		}
	}
}

func graphqlField(toks []token, i int) string {
	var b strings.Builder
	line := toks[i].line
	for k := i; k < len(toks) && toks[k].line == line; k++ {
		b.WriteString(toks[k].text)
		if toks[k].text == ":" || toks[k].text == "," {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

var (
	createTable = regexp.MustCompile(`(?is)\bcreate\s+(?:or\s+replace\s+)?(?:(?:global\s+|local\s+)?(?:temporary|temp)\s+|unlogged\s+|virtual\s+|materialized\s+)?(table|view)\s+(?:if\s+not\s+exists\s+)?([\w."` + "`" + `\[\]]+)`)
	references  = regexp.MustCompile(`(?i)\breferences\s+([\w."` + "`" + `\[\]]+)`)
)

// extractSQL reads DDL: CREATE TABLE and CREATE VIEW become table nodes
// keyed by lower-case name, and a foreign key's REFERENCES is a
// RefContract from the table to the one it references.
func extractSQL(f *facts.File, src []byte) {
	text := string(src)
	locs := createTable.FindAllStringSubmatchIndex(text, -1)
	for n, loc := range locs {
		name := TableKey(text[loc[4]:loc[5]])
		if name == "" {
			continue
		}
		line := 1 + strings.Count(text[:loc[0]], "\n")
		end := len(text)
		if n+1 < len(locs) {
			end = locs[n+1][0]
		}
		body := text[loc[0]:end]
		if semi := strings.Index(body, ";"); semi >= 0 {
			body = body[:semi+1]
		}
		idx := len(f.Symbols)
		f.Symbols = append(f.Symbols, facts.Symbol{Name: name, Kind: facts.KindTable, Line: line,
			EndLine: line + strings.Count(strings.TrimRight(body, "\n"), "\n"), Signature: clip(body),
			Exported: true, Visibility: facts.VisPublic})
		for _, r := range references.FindAllStringSubmatchIndex(body, -1) {
			if to := TableKey(body[r[2]:r[3]]); to != "" && to != name {
				f.Refs = append(f.Refs, facts.Ref{Kind: facts.RefContract, Enclosing: idx, Name: to, Qualifier: facts.KindTable,
					Line: line + strings.Count(body[:r[0]], "\n")})
			}
		}
	}
}

// extractOpenAPI reads an OpenAPI 3 or Swagger 2 document (YAML or JSON):
// each operation is a route node, prefixed by the first server URL's path
// (or basePath), and each schema is a message node.
func extractOpenAPI(f *facts.File, src []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return fmt.Errorf("parse openapi: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	root := doc.Content[0]
	prefix := ""
	if bp := mapGet(root, "basePath"); bp != nil {
		prefix = bp.Value
	}
	if servers := mapGet(root, "servers"); servers != nil && servers.Kind == yaml.SequenceNode && len(servers.Content) > 0 {
		if u := mapGet(servers.Content[0], "url"); u != nil {
			if k := RouteKey("", schemeHost.ReplaceAllString(u.Value, "")); k != "" {
				_, prefix = SplitRoute(k)
			}
		}
	}
	if prefix == "/" {
		prefix = ""
	}
	if info := mapGet(root, "info"); info != nil {
		if t := mapGet(info, "title"); t != nil {
			f.PkgName = t.Value
		}
	}
	if paths := mapGet(root, "paths"); paths != nil && paths.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(paths.Content); i += 2 {
			p, ops := paths.Content[i], paths.Content[i+1]
			if ops.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(ops.Content); j += 2 {
				m, op := ops.Content[j], ops.Content[j+1]
				if !methods[strings.ToUpper(m.Value)] {
					continue
				}
				key := RouteKey(m.Value, JoinPath(prefix, p.Value))
				if key == "" {
					continue
				}
				sig := strings.ToUpper(m.Value) + " " + JoinPath(prefix, p.Value)
				doc := ""
				if id := mapGet(op, "operationId"); id != nil {
					sig += " (" + id.Value + ")"
				}
				if s := mapGet(op, "summary"); s != nil {
					doc = s.Value
				}
				f.Symbols = append(f.Symbols, facts.Symbol{Name: key, Kind: facts.KindRoute, Line: m.Line, EndLine: lastLine(op),
					Signature: clip(sig), Doc: clip(doc), Exported: true, Visibility: facts.VisPublic})
			}
		}
	}
	schemas := mapGet(root, "definitions")
	if c := mapGet(root, "components"); c != nil {
		schemas = mapGet(c, "schemas")
	}
	if schemas != nil && schemas.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(schemas.Content); i += 2 {
			k := schemas.Content[i]
			f.Symbols = append(f.Symbols, facts.Symbol{Name: k.Value, Kind: facts.KindMessage, Line: k.Line, EndLine: lastLine(schemas.Content[i+1]),
				Signature: "schema " + k.Value, Exported: true, Visibility: facts.VisPublic})
		}
	}
	return nil
}

func mapGet(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func lastLine(n *yaml.Node) int {
	line := n.Line
	for _, c := range n.Content {
		line = max(line, lastLine(c))
	}
	return line
}
