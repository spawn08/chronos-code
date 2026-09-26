// Package retrieve selects the code an agent needs for a task, within a
// token budget, from one index snapshot: seeds from exact names, paths and
// BM25; expansion over resolved call edges by local personalized PageRank
// (push method); diversification across files; and packing at three zoom
// levels (excerpt, signature, handle). Every item says why it was chosen.
// See docs/chronos-indexer.md, "Graph retrieval".
package retrieve

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/query"
	"github.com/spawn08/chronos-code/internal/indexer/scan"
)

// Zoom levels, most detailed first.
type Zoom int

const (
	Handle    Zoom = iota // name and location only
	Signature             // plus signature
	Excerpt               // plus a source window inside the declaration
)

func (z Zoom) String() string {
	return [...]string{"handle", "signature", "excerpt"}[z]
}

// Tunables. Push-PPR work is bounded by maxPushes regardless of repository
// size; the estimates only steer packing (the caller certifies the budget
// with an exact count).
const (
	alpha          = 0.25 // teleport probability
	epsilon        = 5e-4 // stop pushing residual below this
	maxPushes      = 600
	maxExpansions  = 80 // nodes whose neighbours are resolved; the rest absorb mass
	maxNeighbors   = 48 // per node and direction
	maxSeedsByName = 8
	searchSeeds    = 12
	maxItems       = 40
	maxExcerpts    = 8
	excerptLines   = 40
	fileDecay      = 0.6 // score multiplier per item already taken from a file
	// Document sections match many task words by sheer length, so text
	// search seeds at most maxSectionSeeds of them, at sectionScale of a
	// code hit's weight and score; their mentions carry mass to code.
	maxSectionSeeds = 2
	sectionScale    = 0.5
	// Documents fill in after code: the best section comes after the
	// first sectionAfter code items, everything else found through
	// documents after all code. Section excerpts are shorter, and given
	// to at most one section, only when the task names no code.
	sectionAfter        = 4
	sectionExcerptLines = 12
	tokensPerLine       = 12
	handleTokens        = 24
)

// expands are the kinds whose outgoing calls are followed: functions, and
// contract nodes, which call their handlers.
var expands = map[string]bool{
	facts.KindFunc: true, facts.KindMethod: true, facts.KindConstructor: true,
	facts.KindRoute: true, facts.KindRPC: true, facts.KindTopic: true,
}

// mentionWeight scales a document mention edge by how it links (backticked
// names and routes are deliberate; bare identifiers are guesses).
var mentionWeight = map[string]float64{
	facts.MentionSymbol: 0.8, facts.MentionRoute: 0.8, facts.MentionPath: 0.5, facts.MentionIdent: 0.4, facts.MentionTicket: 0.3,
}

// Edge weights by resolution; ambiguous weight is split across candidates.
var resolutionWeight = map[string]float64{
	query.TypeChecked: 1, query.ImportResolved: 1, query.TypeHinted: 0.8, query.NameMatched: 0.4, query.Ambiguous: 0.15,
}

// Range is a requested line range of a file.
type Range struct {
	File       string
	Start, End int
}

// Request describes one retrieval.
type Request struct {
	Query    string
	Symbols  []string // exact names
	Ranges   []Range
	Callers  bool // include callers
	Tests    bool // include tests reaching the seeds
	Callees  bool // include callees of the seeds
	Excerpts bool // allow excerpt zoom
	Budget   int  // estimated token budget for the items
	// LineTokens is the estimated tokens per excerpt line in the caller's
	// rendering (JSON escaping costs more than plain text); 0 means 12.
	LineTokens int
	// ItemOverhead is the estimated tokens each item costs in the caller's
	// rendering beyond its signature and excerpt (JSON keys, locations).
	ItemOverhead int
	Seen         *Seen
}

// Item is one selected declaration.
type Item struct {
	Role   string // definition, caller, callee, test, related
	Symbol query.Symbol
	Target string // the seed or neighbour it relates to
	Why    string
	Score  float64
	Zoom   Zoom
	Start  int // excerpt window (Zoom == Excerpt)
	End    int
}

// Result is a selection.
type Result struct {
	Items       []Item
	Misses      []string       // name-like tokens that match nothing
	Invalidated []string       // files delivered earlier whose content changed
	Omitted     map[string]int // counts by reason
	Seeds       int
}

type node struct {
	sym   query.Symbol
	seed  bool
	exact bool // seeded by an exact name, path, range or request
	why   string
	role  string
	pred  uint64 // best predecessor
	edge  string // "calls" (pred calls this) or "called_by" (this calls pred)
	res   string
	best  float64 // mass received from pred
	bm25  float64
}

// Retrieve runs one retrieval over v.
func Retrieve(v *query.View, req Request) Result {
	res := Result{Omitted: map[string]int{}}
	nodes := map[uint64]*node{}
	residual := map[uint64]float64{}
	exact := true
	anchored := false // the task names code: sections come without excerpts (else at most one)
	add := func(s query.Symbol, w float64, why string) {
		n := nodes[s.ID]
		if n == nil {
			n = &node{sym: s, seed: true, why: why, role: "definition"}
			nodes[s.ID] = n
		} else if !n.seed {
			n.seed, n.why, n.role = true, why, "definition"
		}
		n.exact = n.exact || exact
		residual[s.ID] += w
	}
	for _, name := range dedupe(req.Symbols) {
		syms := v.Symbols(name, "")
		if len(syms) == 0 {
			res.Misses = append(res.Misses, name)
			continue
		}
		for _, s := range capSyms(syms, maxSeedsByName) {
			add(s, 1/float64(min(len(syms), maxSeedsByName)), "requested symbol")
		}
	}
	for _, r := range req.Ranges {
		for _, s := range v.SymbolsInRange(r.File, r.Start, r.End) {
			add(s, 1, fmt.Sprintf("declared in %s:%d-%d", r.File, r.Start, r.End))
		}
	}
	if q := strings.TrimSpace(req.Query); q != "" {
		for _, tok := range identifiers(q) {
			if isPath(tok) {
				syms := v.FileSymbols(strings.TrimPrefix(tok, "./"))
				if len(syms) == 0 {
					res.Misses = append(res.Misses, tok)
				}
				for _, s := range capSyms(exportedFirst(syms), maxSeedsByName) {
					add(s, 0.5/float64(min(len(syms), maxSeedsByName)), "declared in "+tok)
				}
				continue
			}
			if !codeShaped(tok) && !unicode.IsUpper([]rune(tok)[0]) {
				// A plain lower-case word may still name an unexported
				// declaration ("validate"): a weaker seed, unless it is a
				// word every task uses.
				if len(tok) < 4 || commonWords[strings.ToLower(tok)] {
					continue
				}
				syms := codeSymbols(v, tok)
				exact = false
				for _, s := range capSyms(syms, maxSeedsByName) {
					add(s, 0.3/float64(min(len(syms), maxSeedsByName)), "name in the task: "+tok)
				}
				exact = true
				continue
			}
			syms := codeSymbols(v, tok)
			if len(syms) == 0 {
				if codeShaped(tok) {
					res.Misses = append(res.Misses, tok)
				}
				continue
			}
			for _, s := range capSyms(syms, maxSeedsByName) {
				add(s, 1/float64(min(len(syms), maxSeedsByName)), "exact name in the task: "+tok)
			}
		}
		exact = false
		limit, scale := searchSeeds, 0.4
		anchored = len(nodes) > 0
		if anchored {
			// Exact anchors exist: text matches only fill in around them.
			limit, scale = 4, 0.1
		}
		hits := codeFirst(v.Search(q, 2*limit+maxSectionSeeds), limit, maxSectionSeeds)
		// Scores are relative to the best code hit: a long section
		// outscoring all code must not shrink the code hits.
		top, topAll := 0.0, 0.0
		for _, h := range hits {
			topAll = math.Max(topAll, h.Score)
			if h.Symbol.Kind != facts.KindSection {
				top = math.Max(top, h.Score)
			}
		}
		if top == 0 {
			top = topAll
		}
		for _, h := range hits {
			// A hit matching only one of several task words is noise.
			if top <= 0 || (h.Terms > 1 && h.Matched < 2 && h.Score < 1000) {
				continue
			}
			w := scale * math.Min(1, h.Score/top)
			if h.Symbol.Kind == facts.KindSection {
				w *= sectionScale
			}
			if nodes[h.ID] == nil {
				add(h.Symbol, w, fmt.Sprintf("matches %d of %d task words", h.Matched, h.Terms))
			} else {
				residual[h.ID] += w
			}
			nodes[h.ID].bm25 = math.Min(1, h.Score/top)
		}
	}
	res.Seeds = len(nodes)
	if len(nodes) == 0 {
		return res
	}
	normalize(residual)

	scores := push(v, req, nodes, residual)
	cands := make([]*node, 0, len(nodes))
	for id, n := range nodes {
		if scores[id] <= 0 && !n.seed {
			continue
		}
		if n.role == "test" && !req.Tests {
			continue
		}
		if n.role == "caller" && !req.Callers {
			continue
		}
		if n.role == "callee" && !req.Callees {
			continue
		}
		cands = append(cands, n)
	}
	score := func(n *node) float64 {
		s := scores[n.sym.ID] + 0.2*n.bm25
		if n.exact {
			s += 1
		}
		if n.role != "test" && n.sym.TestFile && !req.Tests {
			s *= 0.5
		}
		if n.sym.Kind == facts.KindSection {
			s *= sectionScale
		}
		// Two hops away through a non-seed (a callee of a caller): related,
		// but less than direct neighbours.
		if !n.seed && n.pred != 0 && !nodes[n.pred].seed {
			s *= 0.4
		}
		return s
	}
	slices.SortFunc(cands, func(a, b *node) int {
		if c := cmp.Compare(score(b), score(a)); c != 0 {
			return c
		}
		if c := cmp.Compare(a.sym.File, b.sym.File); c != 0 {
			return c
		}
		return cmp.Compare(a.sym.Line, b.sym.Line)
	})
	picked := docsLast(diversify(cands, score))
	if len(picked) > maxItems {
		res.Omitted["item_limit"] += len(picked) - maxItems
		picked = picked[:maxItems]
	}
	pack(v, req, picked, score, nodes, anchored, &res)
	return res
}

// push runs Andersen–Chung–Lang local PageRank from the seed residuals over
// call edges resolved at query time, recording each node's best predecessor.
func push(v *query.View, req Request, nodes map[uint64]*node, residual map[uint64]float64) map[uint64]float64 {
	scores := map[uint64]float64{}
	type edge struct {
		to   query.Symbol
		w    float64
		kind string // calls: u calls to; called_by: to calls u
		res  string
	}
	adj := map[uint64][]edge{}
	neighbors := func(u query.Symbol) []edge {
		if e, ok := adj[u.ID]; ok {
			return e
		}
		if len(adj) >= maxExpansions {
			return nil
		}
		var out []edge
		if expands[u.Kind] {
			seen := map[uint64]bool{}
			for _, c := range v.Outgoing(u) {
				targets, label := v.Resolve(c)
				for _, t := range targets {
					if seen[t.ID] || t.ID == u.ID || len(out) >= maxNeighbors {
						continue
					}
					seen[t.ID] = true
					out = append(out, edge{t, resolutionWeight[label] / float64(len(targets)), "calls", label})
				}
			}
		}
		if u.Kind == facts.KindSection {
			// A document section links to the code it mentions.
			seen := map[uint64]bool{}
			for _, l := range v.Mentions(u) {
				for _, t := range l.Targets {
					if seen[t.ID] || t.ID == u.ID || len(out) >= maxNeighbors {
						continue
					}
					seen[t.ID] = true
					out = append(out, edge{t, mentionWeight[l.Kind] * resolutionWeight[l.Resolution] / float64(len(l.Targets)), "mentions", l.Resolution})
				}
			}
		}
		callers := 0
		for _, in := range v.IncomingCalls(u) {
			if callers >= maxNeighbors {
				break
			}
			callers++
			w := 0.8 * resolutionWeight[in.Resolution] / float64(in.Candidates)
			out = append(out, edge{in.Caller, w, "called_by", in.Resolution})
		}
		adj[u.ID] = out
		return out
	}
	queue := make([]uint64, 0, len(residual))
	for id := range residual {
		queue = append(queue, id)
	}
	slices.Sort(queue)
	for pushes := 0; len(queue) > 0 && pushes < maxPushes; pushes++ {
		id := queue[0]
		queue = queue[1:]
		r := residual[id]
		if r < epsilon {
			continue
		}
		residual[id] = 0
		scores[id] += alpha * r
		n := nodes[id]
		out := neighbors(n.sym)
		total := 0.0
		for _, e := range out {
			total += e.w
		}
		if total == 0 {
			scores[id] += (1 - alpha) * r
			continue
		}
		mass := (1 - alpha) * r
		for _, e := range out {
			m := mass * e.w / total
			t := nodes[e.to.ID]
			if t == nil {
				t = &node{sym: e.to}
				nodes[e.to.ID] = t
			}
			if !t.seed && m > t.best {
				t.best, t.pred, t.edge, t.res = m, id, e.kind, e.res
				t.role = roleFor(e.kind, e.to)
			}
			before := residual[e.to.ID]
			residual[e.to.ID] = before + m
			if before < epsilon && before+m >= epsilon {
				queue = append(queue, e.to.ID)
			}
		}
	}
	for _, n := range nodes {
		if n.seed || n.pred == 0 {
			continue
		}
		p := nodes[n.pred]
		switch n.role {
		case "caller", "test":
			n.why = "calls " + p.sym.Qualified()
		case "callee":
			n.why = "called by " + p.sym.Qualified()
		case "related":
			n.why = "mentioned in " + p.sym.File + ": " + p.sym.Name
		}
		if n.res != "" && n.res != query.ImportResolved {
			n.why += " (" + n.res + ")"
		}
	}
	return scores
}

// docsLast orders code before what documents contributed (sections, and
// declarations reached only through a section's mentions), except that
// the best section follows the first sectionAfter code items: prose must
// not take the budget from code, but a task about the documents should
// still see its section.
func docsLast(picked []*node) []*node {
	var code, docs []*node
	for _, n := range picked {
		if n.sym.Kind == facts.KindSection || n.role == "related" {
			docs = append(docs, n)
		} else {
			code = append(code, n)
		}
	}
	if len(docs) == 0 {
		return picked
	}
	out := make([]*node, 0, len(picked))
	k := min(sectionAfter, len(code))
	out = append(out, code[:k]...)
	var rest []*node
	for _, n := range docs {
		if n.sym.Kind == facts.KindSection && len(out) == k {
			out = append(out, n)
		} else {
			rest = append(rest, n)
		}
	}
	out = append(out, code[k:]...)
	return append(out, rest...)
}

// codeSymbols returns the declarations named name, without document
// sections: a heading that happens to be a task word ("Go", "Search") is
// not an anchor.
func codeSymbols(v *query.View, name string) []query.Symbol {
	syms := v.Symbols(name, "")
	out := syms[:0:0]
	for _, s := range syms {
		if s.Kind != facts.KindSection {
			out = append(out, s)
		}
	}
	return out
}

// codeFirst keeps the first limit code hits and at most maxSections
// document sections, in rank order.
func codeFirst(hits []query.Scored, limit, maxSections int) []query.Scored {
	var out []query.Scored
	code, sections := 0, 0
	for _, h := range hits {
		switch {
		case h.Symbol.Kind == facts.KindSection && sections < maxSections:
			sections++
		case h.Symbol.Kind != facts.KindSection && code < limit:
			code++
		default:
			continue
		}
		out = append(out, h)
	}
	return out
}

func roleFor(kind string, s query.Symbol) string {
	if kind == "mentions" {
		return "related"
	}
	if kind == "called_by" {
		if query.IsTest(s) {
			return "test"
		}
		return "caller"
	}
	return "callee"
}

// diversify re-orders candidates so that each further item from a file
// counts less, spreading the budget across files.
func diversify(cands []*node, score func(*node) float64) []*node {
	perFile := map[string]int{}
	out := make([]*node, 0, len(cands))
	rest := slices.Clone(cands)
	for len(rest) > 0 && len(out) < maxItems*2 {
		best, bestScore := 0, -1.0
		for i, n := range rest {
			s := score(n) * math.Pow(fileDecay, float64(perFile[n.sym.File]))
			if s > bestScore {
				best, bestScore = i, s
			}
		}
		n := rest[best]
		rest = slices.Delete(rest, best, best+1)
		perFile[n.sym.File]++
		out = append(out, n)
	}
	return out
}

// pack assigns zoom levels greedily by score within the estimated budget,
// subtracting source ranges the session has already been given.
func pack(v *query.View, req Request, picked []*node, score func(*node) float64, nodes map[uint64]*node, anchored bool, res *Result) {
	budget := req.Budget
	if budget <= 0 {
		budget = 4096
	}
	excerpts, sectionExcerpts := 0, 0
	overhead := max(req.ItemOverhead, handleTokens)
	invalidated := map[string]bool{}
	for _, n := range picked {
		s := n.sym
		it := Item{Role: n.role, Symbol: s, Why: n.why, Score: score(n)}
		if n.pred != 0 && !n.seed {
			it.Target = nodes[n.pred].sym.Qualified()
		}
		meta, _ := v.FileMeta(s.File)
		if req.Seen != nil && req.Seen.Changed(s.File, meta.Hash) && !invalidated[s.File] {
			invalidated[s.File] = true
			res.Invalidated = append(res.Invalidated, s.File)
		}
		sig := estimate(s.Signature) + estimate(it.Why) + overhead
		lines := excerptLines
		if s.Kind == facts.KindSection {
			lines = sectionExcerptLines
		}
		start, end := s.Line, min(max(s.Line, s.EndLine), s.Line+lines-1)
		lineTokens := req.LineTokens
		if lineTokens <= 0 {
			lineTokens = tokensPerLine
		}
		ex := sig + (end-start+1)*lineTokens
		switch {
		case req.Excerpts && excerpts < maxExcerpts && ex <= budget && (s.Kind != facts.KindSection || !anchored && sectionExcerpts == 0):
			if s.Kind == facts.KindSection {
				sectionExcerpts++
			}
			if req.Seen != nil && req.Seen.Covered(s.File, meta.Hash, start, end) {
				res.Omitted["already_seen"]++
				it.Zoom, it.Why = Signature, it.Why+"; source already delivered in this session"
				budget -= sig
				break
			}
			it.Zoom, it.Start, it.End = Excerpt, start, end
			budget -= ex
			excerpts++
			if end < max(s.Line, s.EndLine) {
				res.Omitted["excerpt_limit"]++
			}
		case sig <= budget:
			it.Zoom = Signature
			budget -= sig
		case overhead <= budget:
			it.Zoom = Handle
			budget -= overhead
		default:
			res.Omitted["budget"]++
			continue
		}
		res.Items = append(res.Items, it)
	}
}

func estimate(s string) int { return (len(s) + 2) / 3 }

func normalize(m map[uint64]float64) {
	total := 0.0
	for _, w := range m {
		total += w
	}
	if total == 0 {
		return
	}
	for k, w := range m {
		m[k] = w / total
	}
}

func capSyms(syms []query.Symbol, n int) []query.Symbol {
	if len(syms) > n {
		return syms[:n]
	}
	return syms
}

func exportedFirst(syms []query.Symbol) []query.Symbol {
	out := slices.Clone(syms)
	slices.SortStableFunc(out, func(a, b query.Symbol) int {
		if a.Exported != b.Exported {
			if a.Exported {
				return -1
			}
			return 1
		}
		return 0
	})
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// identifiers returns the name-like and path-like tokens of task text, in
// order: words of letters, digits, '_', '.', '/' and '-', trimmed of
// sentence punctuation.
func identifiers(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' || r == '/' || r == '-')
	})
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		f = strings.Trim(f, ".-/")
		if len(f) < 2 || seen[f] || !unicode.IsLetter([]rune(f)[0]) && f[0] != '_' && !isPath(f) {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// commonWords are task words too generic to seed by name.
var commonWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`about also make need want like work help used using uses
		code file files test tests find show list read write open close load save call calls check
		change changes update remove delete create build error errors return value values type types
		data this that with from into when what where which there their does done have should could
		would please thanks line lines function functions method methods`) {
		m[w] = true
	}
	return m
}()

func isPath(tok string) bool {
	return strings.Contains(tok, "/") && scan.Indexable(tok)
}

// codeShaped reports whether a token looks like an identifier rather than a
// word: mixed case after the first letter, an underscore, or a dot between
// identifiers. Only such tokens are reported as misses.
func codeShaped(tok string) bool {
	if strings.Contains(tok, "_") {
		return true
	}
	if i := strings.IndexByte(tok, '.'); i > 0 && i < len(tok)-1 {
		return true
	}
	rs := []rune(tok)
	for _, r := range rs[1:] {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}
