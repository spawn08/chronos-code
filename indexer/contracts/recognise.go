package contracts

import (
	_ "embed"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos-code/indexer/facts"
)

//go:embed recognisers.yaml
var recognisersYAML []byte

// Rule is one framework recogniser. Templates ("$1", "$1/$2") expand the
// pattern's groups.
type Rule struct {
	ID    string   `yaml:"id"`
	Langs []string `yaml:"langs"`
	// Kind is the contract kind: route, rpc, topic or table.
	Kind string `yaml:"kind"`
	// Role says what a match means:
	//
	//	serve   the file serves the contract: a node is declared with a
	//	        RefCall to the handler
	//	use     a RefContract from the enclosing function
	//	stub    Var is bound to a client of Service; every Var.M( call is
	//	        a use of Service/M
	//	prefix  a path prefix for the routes of the next class (Scope
	//	        "class") or of the whole file (Scope "file")
	Role string `yaml:"role"`
	// Prefilter: literals of which one must occur in the file for the
	// pattern to run (FoldCase: compared lower-case).
	Prefilter []string `yaml:"prefilter"`
	FoldCase  bool     `yaml:"fold_case"`
	Pattern   string   `yaml:"pattern"`

	Method        string `yaml:"method"`         // route method template
	DefaultMethod string `yaml:"default_method"` // when Method expands to ""; default ANY
	Path          string `yaml:"path"`           // route path template
	Key           string `yaml:"key"`            // rpc ("Service/Method"), topic or table key template
	// Handler of a served contract: "$N" (an expression: name, x.name,
	// pkg.Name), "next" (the definition a decorator or annotation
	// precedes), "enclosing" (the function containing the match) or "".
	Handler string `yaml:"handler"`
	// TypePrefix: when Handler "next" finds a class, the match is a path
	// prefix of that class's routes instead (Spring @RequestMapping).
	TypePrefix bool `yaml:"type_prefix"`
	// EachMethod names a type of this file whose every method serves
	// Service/<method> (gRPC servers).
	EachMethod string `yaml:"each_method"`
	Service    string `yaml:"service"`
	Var        string `yaml:"var"`
	Scope      string `yaml:"scope"`
	// Reject drops a match whose group RejectGroup is one of these words.
	RejectGroup int      `yaml:"reject_group"`
	Reject      []string `yaml:"reject"`

	re *regexp.Regexp
}

type ruleSet struct {
	byLang map[string][]*Rule
	all    []*Rule
}

var (
	rulesOnce sync.Once
	rules     *ruleSet
)

func defaultRules() *ruleSet {
	rulesOnce.Do(func() {
		rs, err := LoadRules(recognisersYAML)
		if err != nil {
			panic(fmt.Sprintf("contracts: embedded recognisers are invalid: %v", err))
		}
		rules = rs
	})
	return rules
}

// Rules returns the embedded recognisers, for tests and diagnostics.
func Rules() []*Rule { return defaultRules().all }

// LoadRules parses and validates recogniser rules.
func LoadRules(data []byte) (*ruleSet, error) {
	var doc struct {
		Rules []*Rule `yaml:"rules"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse recognisers: %w", err)
	}
	rs := &ruleSet{byLang: map[string][]*Rule{}}
	for _, r := range doc.Rules {
		if err := r.compile(); err != nil {
			return nil, err
		}
		rs.all = append(rs.all, r)
		for _, l := range r.Langs {
			rs.byLang[l] = append(rs.byLang[l], r)
		}
	}
	return rs, nil
}

func (r *Rule) compile() error {
	if r.ID == "" || len(r.Langs) == 0 || r.Pattern == "" || len(r.Prefilter) == 0 {
		return fmt.Errorf("recogniser %q: id, langs, prefilter and pattern are required", r.ID)
	}
	re, err := regexp.Compile(r.Pattern)
	if err != nil {
		return fmt.Errorf("recogniser %s: %w", r.ID, err)
	}
	r.re = re
	switch r.Role {
	case "serve", "use":
		if !facts.ContractKinds[r.Kind] || r.Kind == facts.KindMessage {
			return fmt.Errorf("recogniser %s: kind must be route, rpc, topic or table", r.ID)
		}
		if r.Kind == facts.KindRoute && r.Path == "" || r.Kind != facts.KindRoute && r.Key == "" && r.EachMethod == "" {
			return fmt.Errorf("recogniser %s: a route needs path, other kinds key or each_method", r.ID)
		}
	case "stub":
		if r.Var == "" || r.Service == "" {
			return fmt.Errorf("recogniser %s: stub needs var and service", r.ID)
		}
	case "prefix":
		if r.Path == "" || (r.Scope != "file" && r.Scope != "class") {
			return fmt.Errorf("recogniser %s: prefix needs path and scope file or class", r.ID)
		}
	default:
		return fmt.Errorf("recogniser %s: unknown role %q", r.ID, r.Role)
	}
	if r.FoldCase {
		for i, p := range r.Prefilter {
			r.Prefilter[i] = strings.ToLower(p)
		}
	}
	return nil
}

// fileCtx is what recognisers need to know about one file.
type fileCtx struct {
	f      *facts.File
	src    string
	lower  string // src lower-cased, on demand
	lines  []int  // byte offset of each line start
	nSyms  int    // symbols extracted before recognition
	binds  map[string]string
	served map[string]bool
}

// Recognize runs the framework recognisers of f.Lang over src and appends
// the contract nodes the file serves (with a RefCall to each handler) and
// the RefContract uses it makes. f.Symbols must already hold the file's
// declarations with line ranges.
func Recognize(f *facts.File, src []byte) {
	rs := defaultRules().byLang[f.Lang]
	if len(rs) == 0 {
		return
	}
	c := &fileCtx{f: f, src: string(src), nSyms: len(f.Symbols), served: map[string]bool{}}
	var active []*Rule
	for _, r := range rs {
		if c.prefiltered(r) {
			active = append(active, r)
		}
	}
	if len(active) == 0 {
		return
	}
	c.lines = append(c.lines, 0)
	for i := 0; i < len(c.src); i++ {
		if c.src[i] == '\n' {
			c.lines = append(c.lines, i+1)
		}
	}
	type prefix struct {
		path       string
		start, end int // line range; whole file when 0
	}
	var prefixes []prefix
	type route struct {
		method, path string
		line         int
		sig          string
		handler      handlerRef
	}
	var routes []route
	for _, r := range active {
		for _, m := range r.re.FindAllStringSubmatchIndex(c.src, -1) {
			if r.RejectGroup > 0 && slices.Contains(r.Reject, strings.ToLower(group(c.src, m, r.RejectGroup))) {
				continue
			}
			line := c.line(m[0])
			sig := clip(firstLine(c.src[m[0]:m[1]]))
			switch r.Role {
			case "prefix":
				p := prefix{path: r.expand(c.src, m, r.Path)}
				if r.Scope == "class" {
					if k := c.nextType(line); k >= 0 {
						p.start, p.end = c.f.Symbols[k].Line, c.f.Symbols[k].EndLine
					} else {
						continue
					}
				}
				prefixes = append(prefixes, p)
			case "stub":
				c.stub(r, m)
			case "use":
				c.use(r, m, line)
			case "serve":
				if r.EachMethod != "" {
					c.eachMethod(r, m, sig)
					continue
				}
				h := c.handler(r, m)
				if r.TypePrefix && h.sym >= 0 && isType(c.f.Symbols[h.sym].Kind) {
					s := c.f.Symbols[h.sym]
					prefixes = append(prefixes, prefix{path: r.expand(c.src, m, r.Path), start: s.Line, end: s.EndLine})
					continue
				}
				if r.Kind == facts.KindRoute {
					routes = append(routes, route{method: r.method(c.src, m), path: r.expand(c.src, m, r.Path), line: line, sig: sig, handler: h})
					continue
				}
				c.serve(r.Kind, r.key(c.src, m), line, sig, h)
			}
		}
	}
	for _, rt := range routes {
		p := rt.path
		// The innermost class prefix, then file prefixes outside it.
		best := -1
		for i, px := range prefixes {
			if px.start > 0 && px.start <= rt.line && rt.line <= px.end && (best < 0 || px.start >= prefixes[best].start) {
				best = i
			}
		}
		if best >= 0 {
			p = JoinPath(prefixes[best].path, p)
		}
		for _, px := range prefixes {
			if px.start == 0 {
				p = JoinPath(px.path, p)
			}
		}
		if p == "" {
			p = "/"
		}
		c.serve(facts.KindRoute, RouteKey(rt.method, p), rt.line, rt.sig, rt.handler)
	}
}

func (c *fileCtx) prefiltered(r *Rule) bool {
	src := c.src
	if r.FoldCase {
		if c.lower == "" {
			c.lower = strings.ToLower(c.src)
		}
		src = c.lower
	}
	for _, p := range r.Prefilter {
		if strings.Contains(src, p) {
			return true
		}
	}
	return false
}

func group(src string, m []int, n int) string {
	if 2*n+1 >= len(m) || m[2*n] < 0 {
		return ""
	}
	return src[m[2*n]:m[2*n+1]]
}

func (r *Rule) expand(src string, m []int, tmpl string) string {
	return strings.TrimSpace(string(r.re.ExpandString(nil, tmpl, src, m)))
}

func (r *Rule) method(src string, m []int) string {
	if v := r.expand(src, m, r.Method); v != "" {
		return v
	}
	return r.DefaultMethod
}

func (r *Rule) key(src string, m []int) string {
	k := r.expand(src, m, r.Key)
	switch r.Kind {
	case facts.KindRPC:
		svc, meth, _ := strings.Cut(k, "/")
		return RPCKey(svc, meth)
	case facts.KindTable:
		return TableKey(k)
	}
	return k
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// line returns the 1-based line of byte offset off.
func (c *fileCtx) line(off int) int {
	return sort.Search(len(c.lines), func(i int) bool { return c.lines[i] > off })
}

func isCallable(kind string) bool {
	return kind == facts.KindFunc || kind == facts.KindMethod || kind == facts.KindConstructor
}

func isType(kind string) bool {
	switch kind {
	case facts.KindClass, facts.KindStruct, facts.KindInterface, facts.KindTrait, facts.KindProtocol, facts.KindType, facts.KindModule:
		return true
	}
	return false
}

// innermost returns the smallest extracted declaration containing line
// that keep accepts, or -1.
func (c *fileCtx) innermost(line int, keep func(string) bool) int {
	best := -1
	for i := 0; i < c.nSyms; i++ {
		s := c.f.Symbols[i]
		if s.Line <= line && line <= max(s.EndLine, s.Line) && keep(s.Kind) {
			if best < 0 || s.Line > c.f.Symbols[best].Line || (s.Line == c.f.Symbols[best].Line && s.EndLine < c.f.Symbols[best].EndLine) {
				best = i
			}
		}
	}
	return best
}

func (c *fileCtx) enclosing(line int) int {
	return c.innermost(line, isCallable)
}

// first returns the first extracted callable or type declared at or after
// line, or -1.
func (c *fileCtx) first(line int) int {
	best := -1
	for i := 0; i < c.nSyms; i++ {
		s := c.f.Symbols[i]
		if s.Line >= line && (isCallable(s.Kind) || isType(s.Kind)) && (best < 0 || s.Line < c.f.Symbols[best].Line) {
			best = i
		}
	}
	return best
}

func (c *fileCtx) nextType(line int) int {
	if k := c.innermost(line, isType); k >= 0 && c.onlyDecorators(line, c.f.Symbols[k].Line) {
		return k
	}
	if k := c.first(line); k >= 0 && isType(c.f.Symbols[k].Kind) && c.onlyDecorators(line+1, c.f.Symbols[k].Line) {
		return k
	}
	return -1
}

// next returns the declaration a decorator or annotation matched at
// [start, end) belongs to: the function whose extent includes it (Java and
// Kotlin annotations are part of the declaration), or the first
// declaration after it when only decorators, annotations, comments and
// blank lines come between (Python decorators), or the class whose
// extent includes it. -1 when none.
func (c *fileCtx) next(start, end int) int {
	line := c.line(start)
	if k := c.innermost(line, isCallable); k >= 0 {
		return k
	}
	after := c.line(c.decoratorEnd(start, end)) + 1
	if k := c.first(line); k >= 0 && c.onlyDecorators(after, c.f.Symbols[k].Line) {
		return k
	}
	if k := c.innermost(line, isType); k >= 0 {
		return k
	}
	return -1
}

// decoratorEnd returns the offset of the parenthesis closing the first one
// opened in [start, end), or end.
func (c *fileCtx) decoratorEnd(start, end int) int {
	open := strings.IndexByte(c.src[start:end], '(')
	if open < 0 {
		return end
	}
	depth := 0
	for i := start + open; i < len(c.src); i++ {
		switch c.src[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return end
}

// onlyDecorators reports whether lines [from, to) hold nothing but
// decorators, annotations, comments and blank lines.
func (c *fileCtx) onlyDecorators(from, to int) bool {
	for l := from; l < to; l++ {
		if l < 1 || l > len(c.lines) {
			return false
		}
		end := len(c.src)
		if l < len(c.lines) {
			end = c.lines[l]
		}
		t := strings.TrimSpace(c.src[c.lines[l-1]:end])
		if t == "" || t[0] == '@' || t[0] == '#' || t[0] == '*' || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") || t[0] == ')' {
			continue
		}
		return false
	}
	return true
}

type handlerRef struct {
	sym        int // extracted declaration, or -1
	name, qual string
	qualKind   uint8
}

var inlineMarkers = []string{"=>", "func(", "func (", "function", "lambda", "{"}

func (c *fileCtx) handler(r *Rule, m []int) handlerRef {
	none := handlerRef{sym: -1}
	switch h := r.Handler; {
	case h == "":
		return none
	case h == "next":
		if k := c.next(m[0], m[1]); k >= 0 {
			return c.symRef(k)
		}
		return none
	case h == "enclosing":
		if k := c.enclosing(c.line(m[0])); k >= 0 {
			return c.symRef(k)
		}
		return none
	case strings.HasPrefix(h, "$"):
		n, err := strconv.Atoi(strings.Trim(h[1:], "{}"))
		if err != nil || 2*n+1 >= len(m) || m[2*n] < 0 {
			return none
		}
		expr := c.src[m[2*n]:m[2*n+1]]
		switch expr {
		case "func", "function", "lambda", "async", "new":
			return none
		}
		// An inline function between the path and the "handler" means the
		// match's last name is inside its body.
		prev := m[0]
		for g := 1; g < n; g++ {
			if e := m[2*g+1]; e >= 0 && e <= m[2*n] {
				prev = max(prev, e)
			}
		}
		for _, mk := range inlineMarkers {
			if strings.Contains(c.src[prev:m[2*n]], mk) {
				return none
			}
		}
		return c.exprRef(expr)
	}
	return none
}

func (c *fileCtx) symRef(k int) handlerRef {
	s := c.f.Symbols[k]
	h := handlerRef{sym: k, name: s.Name}
	if s.Receiver != "" {
		h.qual, h.qualKind = facts.BaseType(s.Receiver), facts.QualExpr
	}
	return h
}

// exprRef turns a handler expression (getUser, users.get, s.handleX,
// handlers.GetUser) into a reference, qualified by an import when its
// qualifier names one.
func (c *fileCtx) exprRef(expr string) handlerRef {
	expr = strings.TrimSpace(expr)
	dot := strings.LastIndexByte(expr, '.')
	if dot < 0 {
		return handlerRef{sym: -1, name: expr}
	}
	qual, name := expr[:dot], expr[dot+1:]
	if c.binds == nil {
		c.binds = importBindings(c.f.Imports)
	}
	if spec, ok := c.binds[qual]; ok {
		return handlerRef{sym: -1, name: name, qual: spec, qualKind: facts.QualPackage}
	}
	return handlerRef{sym: -1, name: name, qual: qual, qualKind: facts.QualExpr}
}

func importBindings(imps []facts.Import) map[string]string {
	out := map[string]string{}
	for _, imp := range imps {
		switch {
		case imp.Name != "" && imp.Name != "_" && imp.Name != ".":
			out[imp.Name] = imp.Path
		case len(imp.Names) > 0:
			for _, n := range imp.Names {
				if n.Alias != "" {
					out[n.Alias] = imp.Path
				} else if n.Name != "" {
					out[n.Name] = imp.Path
				}
			}
		default:
			p := strings.TrimSuffix(imp.Path, "/")
			if i := strings.LastIndexAny(p, "/."); i >= 0 {
				p = p[i+1:]
			}
			if p != "" {
				out[p] = imp.Path
			}
		}
	}
	return out
}

// serve declares a contract node at line with a RefCall to its handler.
func (c *fileCtx) serve(kind, key string, line int, sig string, h handlerRef) {
	if key == "" {
		return
	}
	id := kind + "\x00" + key + "\x00" + strconv.Itoa(line)
	if c.served[id] {
		return
	}
	c.served[id] = true
	idx := len(c.f.Symbols)
	c.f.Symbols = append(c.f.Symbols, facts.Symbol{Name: key, Kind: kind, Line: line, EndLine: line, Signature: sig,
		Exported: true, Visibility: facts.VisPublic})
	if h.name != "" {
		c.f.Refs = append(c.f.Refs, facts.Ref{Kind: facts.RefCall, Enclosing: idx, Name: h.name, Qualifier: h.qual, QualKind: h.qualKind, Line: line})
	}
}

func (c *fileCtx) use(r *Rule, m []int, line int) {
	var key string
	if r.Kind == facts.KindRoute {
		key = RouteKey(r.method(c.src, m), r.expand(c.src, m, r.Path))
	} else {
		key = r.key(c.src, m)
	}
	if key == "" {
		return
	}
	c.f.Refs = append(c.f.Refs, facts.Ref{Kind: facts.RefContract, Enclosing: c.enclosingOrNone(line), Name: key, Qualifier: r.Kind, Line: line,
		Col: m[0] - c.lines[line-1] + 1})
}

func (c *fileCtx) enclosingOrNone(line int) int {
	if k := c.enclosing(line); k >= 0 {
		return k
	}
	return facts.NoCaller
}

// eachMethod declares Service/<method> for every method of the type the
// rule names, at the method, handled by it.
func (c *fileCtx) eachMethod(r *Rule, m []int, sig string) {
	typ, svc := r.expand(c.src, m, r.EachMethod), r.expand(c.src, m, r.Service)
	if typ == "" || svc == "" {
		return
	}
	for k := 0; k < c.nSyms; k++ {
		s := c.f.Symbols[k]
		if s.Kind != facts.KindMethod || facts.BaseType(s.Receiver) != typ || strings.HasPrefix(s.Name, "_") || s.Name == "mustEmbedUnimplemented"+svc+"Server" {
			continue
		}
		c.serve(r.Kind, RPCKey(svc, s.Name), s.Line, sig, c.symRef(k))
	}
}

// stub records a use of Service/M for every call v.M( of the bound
// client variable v (its last segment: s.client binds "client").
func (c *fileCtx) stub(r *Rule, m []int) {
	v, svc := r.expand(c.src, m, r.Var), r.expand(c.src, m, r.Service)
	if i := strings.LastIndexByte(v, '.'); i >= 0 {
		v = v[i+1:]
	}
	if v == "" || svc == "" {
		return
	}
	calls := regexp.MustCompile(`(?:^|[^\w])` + regexp.QuoteMeta(v) + `\s*\.\s*(\w+)\s*\(`)
	for _, cm := range calls.FindAllStringSubmatchIndex(c.src, -1) {
		meth := c.src[cm[2]:cm[3]]
		if meth == "close" || meth == "Close" {
			continue
		}
		line := c.line(cm[2])
		c.f.Refs = append(c.f.Refs, facts.Ref{Kind: facts.RefContract, Enclosing: c.enclosingOrNone(line), Name: RPCKey(svc, meth),
			Qualifier: facts.KindRPC, Line: line, Col: cm[2] - c.lines[line-1] + 1})
	}
}
