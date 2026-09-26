// Package generic extracts syntactic facts from one source file with a
// language pack's tree-sitter query (see package packs for the capture
// names). The query says what the definitions, imports, exports and
// references are; this package derives the rest the same way for every
// language: nesting and receivers from source ranges, doc comments from
// the comments before a definition, signatures from the text before the
// body, and visibility and modifiers from the header words and the pack's
// rules. No other file is read.
package generic

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	gts "github.com/odvcencio/gotreesitter"

	"github.com/spawn08/chronos-code/internal/indexer/extract/packs"
	"github.com/spawn08/chronos-code/internal/indexer/extract/treesitter"
	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

const (
	maxDocBytes       = 1024
	maxSignatureRunes = 200
	maxSignatureScan  = 2048 // bytes of source examined for a signature
	maxQualifierBytes = 64
	maxHeaderWords    = 64
	generatedScan     = 1024 // bytes checked for a generated-code marker

	// Files above maxParseBytes, and minified files (at least
	// minifiedMinBytes with an average line longer than minifiedLineBytes),
	// are recorded without symbols: parsing them costs seconds and yields
	// nothing an agent reads.
	maxParseBytes     = 1 << 20
	minifiedMinBytes  = 32 << 10
	minifiedLineBytes = 1000
)

// Extractor extracts facts for any pack. It is safe for concurrent use;
// grammars and queries are compiled on first use and kept.
type Extractor struct {
	rt *treesitter.Runtime

	mu      sync.Mutex
	queries map[*packs.Pack]*compiled
}

type compiled struct {
	once sync.Once
	q    *gts.Query
	lang *gts.Language
	err  error
}

// New returns an Extractor that parses with rt.
func New(rt *treesitter.Runtime) *Extractor {
	return &Extractor{rt: rt, queries: map[*packs.Pack]*compiled{}}
}

func (x *Extractor) compile(pk *packs.Pack) *compiled {
	x.mu.Lock()
	c, ok := x.queries[pk]
	if !ok {
		c = &compiled{}
		x.queries[pk] = c
	}
	x.mu.Unlock()
	c.once.Do(func() {
		lang, err := x.rt.Language(pk.Grammar)
		if err != nil {
			c.err = err
			return
		}
		q, err := gts.NewQuery(pk.Query, lang)
		if err != nil {
			c.err = fmt.Errorf("pack %s query: %w", pk.ID, err)
			return
		}
		c.q, c.lang = q, lang
	})
	return c
}

// Check compiles the pack's query, for tests and startup diagnostics.
func (x *Extractor) Check(pk *packs.Pack) error {
	if pk.Query == "" {
		return nil
	}
	return x.compile(pk).err
}

// Extract parses src with the pack's grammar and fills f: Lang, PkgName,
// the Test and Generated flags, Symbols, Imports, Exports and Refs. A parse
// that stops early or contains syntax errors is noted in f.ParseErr;
// whatever parsed is kept, except after a deadline or memory stop, which
// leaves no tree.
func (x *Extractor) Extract(f *facts.File, pk *packs.Pack, src []byte) {
	f.Lang = pk.Language
	f.Test = isTestFile(pk, f.Path)
	f.Generated = isGenerated(src)
	if pk.Query == "" {
		return
	}
	if len(src) > maxParseBytes {
		f.ParseErr = "file too large to parse"
		return
	}
	if isMinified(src) {
		f.Generated, f.ParseErr = true, "minified file not parsed"
		return
	}
	c := x.compile(pk)
	if c.err != nil {
		f.ParseErr = c.err.Error()
		return
	}
	tree, err := x.rt.Parse(pk.Grammar, src)
	if err != nil {
		f.ParseErr = err.Error()
		return
	}
	defer tree.Release()
	root := tree.Root()
	switch {
	case tree.Partial:
		f.ParseErr = fmt.Sprintf("parse stopped at byte %d of %d (error recovery failed)", tree.Covered, len(src))
	case root.HasError():
		f.ParseErr = fmt.Sprintf("syntax error at line %d", firstError(root).StartPoint().Row+1)
	}
	w := &walker{pk: pk, lang: c.lang, src: src, f: f}
	w.run(c.q.ExecuteNode(root, c.lang, src))
}

// def is a definition or a scope (a container that is not a symbol).
type def struct {
	node, outer, name *gts.Node // outer: the outermost wrapper, or node
	receiver, doc     *gts.Node
	body              *gts.Node
	kind              string
	decl, scope       bool
	start, end        uint32 // extent: outer's range
	pattern           int
	sym               int // index into File.Symbols; -1 for scopes
	parent            *def
}

type ref struct {
	kind      uint8
	name      *gts.Node
	qualifier *gts.Node
}

type importGroup struct {
	node *gts.Node
	imp  facts.Import
	pos  []uint32 // source position of each of imp.Names

	pathStart, pathEnd uint32 // span of the @import.path captures
}

type walker struct {
	pk   *packs.Pack
	lang *gts.Language
	src  []byte
	f    *facts.File

	defs     []*def
	refs     []ref
	imports  []*importGroup
	byImport map[[2]uint32]*importGroup
}

func key(n *gts.Node) [2]uint32 { return [2]uint32{n.StartByte(), n.EndByte()} }

func (w *walker) text(n *gts.Node) string {
	if n == nil {
		return ""
	}
	s, e := n.StartByte(), n.EndByte()
	if e > uint32(len(w.src)) || s > e {
		return ""
	}
	return string(w.src[s:e])
}

func (w *walker) run(matches []gts.QueryMatch) {
	// Earlier patterns win when two capture the same definition.
	slices.SortStableFunc(matches, func(a, b gts.QueryMatch) int { return a.PatternIndex - b.PatternIndex })
	w.byImport = map[[2]uint32]*importGroup{}
	seenDef := map[[2]uint32]bool{}
	var exports []gts.QueryMatch
	for _, m := range matches {
		switch main, node := mainCapture(m); {
		case main == "":
		case strings.HasPrefix(main, "def.") || main == "scope":
			if seenDef[key(node)] {
				continue
			}
			if d := w.newDef(m, main, node); d != nil {
				seenDef[key(node)] = true
				w.defs = append(w.defs, d)
			}
		case main == "import" || main == "include":
			w.addImport(m, main, node)
		case main == "export":
			exports = append(exports, m)
		case strings.HasPrefix(main, "ref."):
			w.addRef(m, main)
		case main == "package":
			if w.f.PkgName == "" {
				w.f.PkgName = strings.TrimSuffix(collapse(w.text(node)), ";")
			}
		}
	}
	w.symbols()
	slices.SortStableFunc(exports, func(a, b gts.QueryMatch) int {
		_, na := mainCapture(a)
		_, nb := mainCapture(b)
		return int(na.StartByte()) - int(nb.StartByte())
	})
	for _, m := range exports {
		w.addExport(m)
	}
	slices.SortStableFunc(w.imports, func(a, b *importGroup) int { return int(a.node.StartByte()) - int(b.node.StartByte()) })
	for _, g := range w.imports {
		if g.imp.Path != "" || len(g.imp.Names) > 0 {
			w.f.Imports = append(w.f.Imports, g.imp)
		}
	}
	w.references()
}

// mainCapture returns the capture that says what a match is.
func mainCapture(m gts.QueryMatch) (string, *gts.Node) {
	for _, c := range m.Captures {
		switch n := c.Name; {
		case n == "ref.qualifier":
		case strings.HasPrefix(n, "def."), strings.HasPrefix(n, "ref."),
			n == "scope", n == "import", n == "include", n == "export", n == "package":
			return n, c.Node
		}
	}
	return "", nil
}

func capture(m gts.QueryMatch, name string) *gts.Node {
	for _, c := range m.Captures {
		if c.Name == name {
			return c.Node
		}
	}
	return nil
}

func (w *walker) newDef(m gts.QueryMatch, main string, node *gts.Node) *def {
	d := &def{node: node, pattern: m.PatternIndex, sym: -1}
	if main == "scope" {
		d.scope, d.name = true, capture(m, "scope.name")
	} else {
		d.kind = strings.TrimPrefix(main, "def.")
		d.kind, d.decl = strings.CutSuffix(d.kind, ".decl")
		d.name = capture(m, "name")
		if d.name == nil || !slices.Contains(facts.Kinds, d.kind) || d.kind == facts.KindEmbed {
			return nil
		}
		d.receiver, d.doc, d.body = capture(m, "receiver"), capture(m, "doc"), capture(m, "body")
	}
	d.outer = node
	for p := node.Parent(); p != nil && slices.Contains(w.pk.Wrappers, p.Type(w.lang)); p = p.Parent() {
		d.outer = p
	}
	d.start, d.end = d.outer.StartByte(), d.outer.EndByte()
	// Attributes and annotations before it (Rust, Dart) belong to it.
	for sib := d.outer.PrevSibling(); sib != nil && slices.Contains(w.pk.DocSkip, sib.Type(w.lang)); sib = sib.PrevSibling() {
		d.start = sib.StartByte()
	}
	// A body next to the definition, not inside it (Dart signatures).
	if d.body != nil && d.body.EndByte() > d.end {
		d.end = d.body.EndByte()
	}
	return d
}

// byRange orders definitions by start, outer ones first.
func byRange(start func(*def) uint32, end func(*def) uint32) func(a, b *def) int {
	return func(a, b *def) int {
		if start(a) != start(b) {
			return int(start(a)) - int(start(b))
		}
		return int(end(b)) - int(end(a))
	}
}

func nodeStart(d *def) uint32 { return d.node.StartByte() }
func nodeEnd(d *def) uint32   { return d.node.EndByte() }

// symbols orders definitions by position, nests them by their node ranges
// (not their wrappers': two declarators share one declaration) and records
// them as symbols.
func (w *walker) symbols() {
	slices.SortStableFunc(w.defs, byRange(nodeStart, nodeEnd))
	var stack []*def
	for _, d := range w.defs {
		for len(stack) > 0 && nodeEnd(stack[len(stack)-1]) <= nodeStart(d) {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 {
			d.parent = stack[len(stack)-1]
		}
		stack = append(stack, d)
		if d.scope {
			continue
		}
		w.symbol(d)
	}
}

func (w *walker) memberOf(kind string) bool {
	if len(w.pk.MemberOf) > 0 {
		return slices.Contains(w.pk.MemberOf, kind)
	}
	switch kind {
	case facts.KindClass, facts.KindStruct, facts.KindInterface, facts.KindTrait, facts.KindProtocol, facts.KindEnum:
		return true
	}
	return false
}

func (w *walker) symbol(d *def) {
	s := facts.Symbol{
		Name: strings.TrimPrefix(w.text(d.name), ":"), Kind: d.kind, // Ruby :symbol names
		Line:    int(d.node.StartPoint().Row) + 1,
		EndLine: endLine(d.node),
	}
	if d.body != nil && d.body.EndByte() > d.node.EndByte() {
		s.EndLine = endLine(d.body)
	}
	var container *def
	for p := d.parent; p != nil; p = p.parent {
		if !p.scope {
			container = p
			break
		}
	}
	if container != nil {
		s.Container = container.sym + 1
	}
	// The owner of a member: an explicit receiver, the nearest scope, or a
	// type-like container.
	switch owner := d.parent; {
	case d.receiver != nil:
		s.Receiver = collapse(w.text(d.receiver))
	case owner != nil && owner.scope:
		s.Receiver = collapse(w.text(owner.name))
	case owner != nil && w.memberOf(w.f.Symbols[owner.sym].Kind):
		s.Receiver = w.f.Symbols[owner.sym].Name
	}
	switch {
	case s.Kind == facts.KindFunc && s.Receiver != "":
		s.Kind = facts.KindMethod
	case s.Kind == facts.KindMethod && s.Receiver == "":
		s.Kind = facts.KindFunc
	}
	if d.decl {
		s.Modifiers |= facts.ModDecl
	}
	words := w.headerWords(d)
	s.Visibility = w.visibility(d, s, container, words)
	s.Exported = s.Visibility == facts.VisPublic
	for _, word := range words {
		switch w.pk.Modifiers[word] {
		case "static":
			s.Modifiers |= facts.ModStatic
		case "abstract":
			s.Modifiers |= facts.ModAbstract
		case "async":
			s.Modifiers |= facts.ModAsync
		case "override":
			s.Modifiers |= facts.ModOverride
		case "deprecated":
			s.Modifiers |= facts.ModDeprecated
		}
	}
	if (s.Kind == facts.KindFunc || s.Kind == facts.KindMethod) && w.isTestSymbol(s.Name, words) {
		s.Modifiers |= facts.ModTest
	}
	s.Signature = w.signature(d)
	s.Doc = w.docFor(d)
	if strings.Contains(s.Doc, "@deprecated") { // JSDoc, Javadoc, PHPDoc
		s.Modifiers |= facts.ModDeprecated
	}
	d.sym = len(w.f.Symbols)
	w.f.Symbols = append(w.f.Symbols, s)
}

func endLine(n *gts.Node) int {
	sp, ep := n.StartPoint(), n.EndPoint()
	if ep.Column == 0 && ep.Row > sp.Row {
		return int(ep.Row)
	}
	return int(ep.Row) + 1
}

// headerWords returns the tokens of a definition before its name,
// including its wrappers' (decorators, export keywords, annotations) and
// the doc_skip siblings before it (Rust attributes).
func (w *walker) headerWords(d *def) []string {
	limit := d.name.StartByte()
	var out []string
	var visit func(n *gts.Node)
	visit = func(n *gts.Node) {
		if len(out) >= maxHeaderWords || n.StartByte() >= limit {
			return
		}
		if n.ChildCount() == 0 {
			switch t := strings.TrimSpace(w.text(n)); {
			case t == "":
			case len(out) > 0 && out[len(out)-1] == "@":
				out[len(out)-1] = "@" + t // annotations and decorators: @Test
			default:
				out = append(out, t)
			}
			return
		}
		for i := 0; i < n.ChildCount(); i++ {
			visit(n.Child(i))
		}
	}
	if len(w.pk.DocSkip) > 0 {
		for sib := d.outer.PrevSibling(); sib != nil; sib = sib.PrevSibling() {
			if t := sib.Type(w.lang); slices.Contains(w.pk.DocSkip, t) {
				visit(sib)
			} else if !isComment(t) {
				break
			}
		}
	}
	visit(d.outer)
	return out
}

func visibilityValue(name string) uint8 {
	switch name {
	case "public":
		return facts.VisPublic
	case "protected":
		return facts.VisProtected
	case "internal":
		return facts.VisInternal
	case "private":
		return facts.VisPrivate
	case "package":
		return facts.VisPackage
	}
	return facts.VisUnknown
}

func (w *walker) visibility(d *def, s facts.Symbol, container *def, words []string) uint8 {
	v := w.pk.Visibility
	var found uint8
	for _, word := range words {
		if vis := visibilityValue(v.Keywords[word]); vis != facts.VisUnknown {
			found = vis
		}
	}
	if found != facts.VisUnknown {
		return found
	}
	if p := v.PrivatePrefix; p != "" && strings.HasPrefix(s.Name, p) && !isDunder(s.Name) {
		return facts.VisPrivate
	}
	if container != nil && len(v.Labels) > 0 {
		for sib := d.outer.PrevSibling(); sib != nil; sib = sib.PrevSibling() {
			if slices.Contains(v.Labels, sib.Type(w.lang)) {
				label := strings.TrimSuffix(strings.TrimSpace(w.text(sib)), ":")
				if vis := visibilityValue(v.Keywords[label]); vis != facts.VisUnknown {
					return vis
				}
			}
		}
	}
	if v.ExportWrapper != "" {
		for p := d.node.Parent(); p != nil && p.StartByte() >= d.outer.StartByte(); p = p.Parent() {
			if p.Type(w.lang) == v.ExportWrapper {
				return facts.VisPublic
			}
			if p == d.outer {
				break
			}
		}
	}
	if container != nil {
		kind := w.f.Symbols[container.sym].Kind
		if vis, ok := v.Members[kind]; ok {
			return visibilityValue(vis)
		}
		if vis, ok := v.Members["*"]; ok {
			return visibilityValue(vis)
		}
	}
	return visibilityValue(v.Default)
}

// isTestSymbol: a name glob in a test file, or a marker word anywhere
// (Rust's #[test] functions live next to the code they test).
// isDunder reports Python special names (__init__), which are public.
func isDunder(name string) bool {
	return len(name) > 4 && strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__")
}

func (w *walker) isTestSymbol(name string, words []string) bool {
	for _, g := range w.pk.Tests.Symbols {
		if ok, _ := path.Match(g, name); ok && w.f.Test {
			return true
		}
	}
	for _, word := range words {
		if slices.Contains(w.pk.Tests.Markers, word) {
			return true
		}
	}
	return false
}

// signature is the definition's text, from its outermost wrapper (so a
// field keeps its type and modifiers) up to its body, on one line.
func (w *walker) signature(d *def) string {
	start, end := d.outer.StartByte(), d.node.EndByte()
	body := d.body
	if body == nil {
		body = d.node.ChildByFieldName("body", w.lang)
	}
	if body != nil && body.StartByte() > start && body.StartByte() <= end {
		end = body.StartByte()
	}
	// Some grammars attach a comment between the header and the body.
	for i := 0; i < d.node.ChildCount(); i++ {
		if c := d.node.Child(i); c.StartByte() > d.name.StartByte() && c.StartByte() < end && isComment(c.Type(w.lang)) {
			end = c.StartByte()
			break
		}
	}
	end = min(end, start+maxSignatureScan, uint32(len(w.src)))
	s := collapse(string(w.src[start:end]))
	s = strings.TrimRight(s, " {:;=")
	if utf8.RuneCountInString(s) > maxSignatureRunes {
		r := []rune(s)
		s = string(r[:maxSignatureRunes-3]) + "..."
	}
	return s
}

func isComment(t string) bool { return strings.Contains(t, "comment") }

// docFor returns the definition's doc: an explicit @doc node, or the
// comments directly before it (and its wrappers), with comment markers
// removed.
func (w *walker) docFor(d *def) string {
	if d.doc != nil {
		return cleanDoc(w.text(d.doc))
	}
	var parts []string
	next := d.outer.StartPoint().Row
	for sib := d.outer.PrevSibling(); sib != nil; sib = sib.PrevSibling() {
		t := sib.Type(w.lang)
		if slices.Contains(w.pk.DocSkip, t) {
			next = sib.StartPoint().Row
			continue
		}
		if !isComment(t) || sib.EndPoint().Row+1 < next {
			break
		}
		// A comment trailing code on its own line documents that code.
		if prev := sib.PrevSibling(); prev != nil && !isComment(prev.Type(w.lang)) && prev.EndPoint().Row == sib.StartPoint().Row {
			break
		}
		parts = append(parts, w.text(sib))
		next = sib.StartPoint().Row
	}
	slices.Reverse(parts)
	return cleanDoc(strings.Join(parts, "\n"))
}

// cleanDoc strips comment markers and string quotes, line by line.
func cleanDoc(s string) string {
	s = strings.TrimSpace(s)
	for _, q := range []string{`"""`, `'''`} {
		if strings.HasPrefix(strings.TrimLeft(s, "rRbBuU"), q) && strings.HasSuffix(s, q) && len(s) >= 6 {
			s = strings.TrimLeft(s, "rRbBuU")
			s = s[3 : len(s)-3]
		}
	}
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		l = strings.TrimSpace(l)
		for _, p := range []string{"/**", "/*!", "/*", "///", "//!", "//", "#", "--"} {
			if strings.HasPrefix(l, p) {
				l = l[len(p):]
				break
			}
		}
		l = strings.TrimPrefix(strings.TrimSuffix(l, "*/"), "*")
		out = append(out, strings.TrimSpace(l))
	}
	s = strings.TrimSpace(strings.Join(out, "\n"))
	if len(s) > maxDocBytes {
		cut := maxDocBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

func (w *walker) addImport(m gts.QueryMatch, main string, node *gts.Node) {
	g, ok := w.byImport[key(node)]
	if !ok {
		g = &importGroup{node: node, imp: facts.Import{Line: int(node.StartPoint().Row) + 1}}
		if main == "include" {
			g.imp.Kind = facts.ImportInclude
		}
		w.byImport[key(node)] = g
		w.imports = append(w.imports, g)
	}
	// The path may be split over several nodes (Scala's a.b.c is three
	// path fields); it spans all of them.
	for _, c := range m.Captures {
		if c.Name != "import.path" {
			continue
		}
		if g.pathEnd == 0 || c.Node.StartByte() < g.pathStart {
			g.pathStart = c.Node.StartByte()
		}
		g.pathEnd = max(g.pathEnd, c.Node.EndByte())
		g.imp.Path = unquote(collapse(string(w.src[g.pathStart:g.pathEnd])))
	}
	if capture(m, "import.wildcard") != nil {
		g.imp.Kind = facts.ImportWildcard
	}
	alias := unquote(w.text(capture(m, "import.alias")))
	switch name, def := capture(m, "import.name"), capture(m, "import.default"); {
	case name != nil:
		g.addName(facts.ImportedName{Name: unquote(collapse(w.text(name))), Alias: alias}, name.StartByte())
	case def != nil:
		g.addName(facts.ImportedName{Name: "default", Alias: w.text(def)}, def.StartByte())
	case alias != "":
		g.imp.Name = alias
	}
}

// addName inserts a listed name in source order.
func (g *importGroup) addName(n facts.ImportedName, pos uint32) {
	if n.Name == "" || slices.Contains(g.imp.Names, n) {
		return
	}
	i, _ := slices.BinarySearch(g.pos, pos)
	g.pos = slices.Insert(g.pos, i, pos)
	g.imp.Names = slices.Insert(g.imp.Names, i, n)
}

func (w *walker) addExport(m gts.QueryMatch) {
	_, node := mainCapture(m)
	e := facts.Export{Line: int(node.StartPoint().Row) + 1, Source: unquote(w.text(capture(m, "export.source")))}
	switch name, def := capture(m, "export.name"), capture(m, "export.default"); {
	case capture(m, "export.all") != nil:
		e.Name = "*"
	case def != nil:
		e.Name, e.SourceName = "default", w.text(def)
	case name != nil:
		e.Name = unquote(w.text(name))
		if alias := capture(m, "export.alias"); alias != nil {
			e.Name, e.SourceName = unquote(w.text(alias)), e.Name
		}
	}
	if e.Name != "" && !slices.Contains(w.f.Exports, e) {
		w.f.Exports = append(w.f.Exports, e)
	}
	if e.Source == "" {
		return
	}
	// The import side of a re-export: one per statement.
	if _, ok := w.byImport[key(node)]; !ok {
		g := &importGroup{node: node, imp: facts.Import{Path: e.Source, Kind: facts.ImportReexport, Line: e.Line}}
		w.byImport[key(node)] = g
		w.imports = append(w.imports, g)
	}
}

var refKinds = map[string]uint8{
	"ref.call": facts.RefCall, "ref.type": facts.RefTypeUse, "ref.extends": facts.RefExtends,
	"ref.implements": facts.RefImplements, "ref.instantiate": facts.RefInstantiate, "ref.decorator": facts.RefDecorator,
}

var refPriority = map[uint8]int{
	facts.RefExtends: 0, facts.RefImplements: 0, facts.RefDecorator: 1,
	facts.RefInstantiate: 2, facts.RefCall: 3, facts.RefTypeUse: 4,
}

func (w *walker) addRef(m gts.QueryMatch, main string) {
	kind, ok := refKinds[main]
	name := capture(m, "name")
	if !ok || name == nil {
		return
	}
	w.refs = append(w.refs, ref{kind: kind, name: name, qualifier: capture(m, "ref.qualifier")})
}

// references records refs in source order, each with its innermost
// enclosing symbol and its qualifier classified against the imports.
func (w *walker) references() {
	if len(w.refs) == 0 {
		return
	}
	binds := w.bindings()
	// One reference per name position; when patterns overlap (a decorator
	// is also a call, new T() also names a type) the specific kind wins.
	slices.SortStableFunc(w.refs, func(a, b ref) int {
		if a.name.StartByte() != b.name.StartByte() {
			return int(a.name.StartByte()) - int(b.name.StartByte())
		}
		return refPriority[a.kind] - refPriority[b.kind]
	})
	// A reference belongs to the innermost definition whose extent (with
	// decorators and other wrappers) contains it.
	defs := slices.Clone(w.defs)
	slices.SortStableFunc(defs, byRange(func(d *def) uint32 { return d.start }, func(d *def) uint32 { return d.end }))
	seen := map[uint32]bool{}
	var stack []*def
	next := 0
	for _, r := range w.refs {
		pos := r.name.StartByte()
		if seen[pos] {
			continue
		}
		seen[pos] = true
		for next < len(defs) && defs[next].start <= pos {
			d := defs[next]
			for len(stack) > 0 && stack[len(stack)-1].end <= d.start {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, d)
			next++
		}
		for len(stack) > 0 && stack[len(stack)-1].end <= pos {
			stack = stack[:len(stack)-1]
		}
		enclosing := facts.NoCaller
		for i := len(stack) - 1; i >= 0; i-- {
			if !stack[i].scope && stack[i].sym >= 0 {
				enclosing = stack[i].sym
				break
			}
		}
		name, qual := splitQualified(collapse(w.text(r.name)))
		if r.qualifier != nil {
			qual = collapse(w.text(r.qualifier))
		}
		if name == "" {
			continue
		}
		out := facts.Ref{
			Kind: r.kind, Enclosing: enclosing, Name: name,
			Line: int(r.name.StartPoint().Row) + 1, Col: int(r.name.StartPoint().Column) + 1,
		}
		switch spec, ok := binds[qual]; {
		case qual == "":
		case ok:
			out.QualKind, out.Qualifier = facts.QualPackage, spec
		default:
			if len(qual) > maxQualifierBytes {
				qual = qual[:maxQualifierBytes-3] + "..."
			}
			out.QualKind, out.Qualifier = facts.QualExpr, qual
		}
		w.f.Refs = append(w.f.Refs, out)
	}
}

// bindings maps the local names imports bind to their specs.
func (w *walker) bindings() map[string]string {
	out := map[string]string{}
	seps := w.pk.Imports.Separators
	if seps == "" {
		seps = "."
	}
	for _, imp := range w.f.Imports {
		if imp.Kind == facts.ImportInclude || imp.Kind == facts.ImportReexport {
			continue
		}
		if imp.Name != "" && imp.Name != "_" && imp.Name != "." {
			out[imp.Name] = imp.Path
		}
		for _, n := range imp.Names {
			local := n.Alias
			if local == "" {
				local = n.Name
			}
			if local != "" && local != "*" {
				out[local] = imp.Path
			}
		}
		if imp.Name != "" || len(imp.Names) > 0 || imp.Kind == facts.ImportWildcard || imp.Path == "" {
			continue
		}
		segs := strings.FieldsFunc(imp.Path, func(r rune) bool { return strings.ContainsRune(seps, r) })
		if len(segs) == 0 {
			continue
		}
		switch w.pk.Imports.Binds {
		case "none":
			continue
		case "first":
			out[segs[0]] = imp.Path
		default:
			out[segs[len(segs)-1]] = imp.Path
		}
		out[imp.Path] = imp.Path
	}
	return out
}

// splitQualified splits "a.b.c" or "a::b::c" into ("c", "a.b").
func splitQualified(s string) (name, qual string) {
	if i := strings.IndexAny(s, "<("); i > 0 {
		s = s[:i]
	}
	best, width := -1, 0
	for _, sep := range []string{"::", ".", "->", "\\"} {
		if i := strings.LastIndex(s, sep); i > best {
			best, width = i, len(sep)
		}
	}
	if best < 0 {
		return s, ""
	}
	return s[best+width:], s[:best]
}

func collapse(s string) string {
	if !strings.ContainsAny(s, "\n\t\r") && !strings.Contains(s, "  ") {
		return strings.TrimSpace(s)
	}
	return strings.Join(strings.Fields(s), " ")
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		switch f, l := s[0], s[len(s)-1]; {
		case (f == '"' || f == '\'' || f == '`') && l == f, f == '<' && l == '>':
			return s[1 : len(s)-1]
		}
	}
	return s
}

func firstError(n *gts.Node) *gts.Node {
	for {
		if n.IsError() || n.IsMissing() {
			return n
		}
		var next *gts.Node
		for i := 0; i < n.ChildCount(); i++ {
			if c := n.Child(i); c.IsError() || c.IsMissing() || c.HasError() {
				next = c
				break
			}
		}
		if next == nil {
			return n
		}
		n = next
	}
}

func isTestFile(pk *packs.Pack, p string) bool {
	base := path.Base(p)
	for _, g := range pk.Tests.Files {
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	if len(pk.Tests.Dirs) > 0 {
		for _, dir := range strings.Split(path.Dir(p), "/") {
			if slices.Contains(pk.Tests.Dirs, dir) {
				return true
			}
		}
	}
	return false
}

func isMinified(src []byte) bool {
	return len(src) >= minifiedMinBytes && len(src)/(bytes.Count(src, []byte{'\n'})+1) > minifiedLineBytes
}

var generatedMarkers = [][]byte{[]byte("DO NOT EDIT"), []byte("@generated"), []byte("<auto-generated")}

// isGenerated looks for a generated-code marker near the top of the file.
func isGenerated(src []byte) bool {
	head := src[:min(len(src), generatedScan)]
	for _, m := range generatedMarkers {
		if bytes.Contains(head, m) {
			return true
		}
	}
	return false
}
