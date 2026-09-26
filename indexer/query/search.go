package query

import (
	"cmp"
	"math"
	"slices"
	"strings"

	"github.com/spawn08/chronos-code/indexer/facts"
	"github.com/spawn08/chronos-code/indexer/segment"
	"github.com/spawn08/chronos-code/indexer/terms"
)

// BM25 parameters.
const (
	bm25K1     = 1.2
	bm25B      = 0.75
	fuzzyLimit = 25
	// A term in more than commonDocs documents (and more than 1/20 of
	// them) carries almost no idf and is skipped when the query has rarer
	// terms; postings visited per term are capped at maxPostings.
	commonDocs  = 50000
	maxPostings = 200000
)

func queryTerms(q string) []string { return terms.Query(q) }

func stem(w string) string { return terms.Stem(w) }

func common(df, n int) bool { return df > commonDocs && df*20 > n }

// Scored is a search hit.
type Scored struct {
	Symbol
	Score   float64 // higher is better
	Matched int     // distinct query terms the symbol matched
	Terms   int     // distinct terms in the query
}

// corpus returns 1 for a document section and 0 for code.
func corpus(seg *segment.Segment, sym int) int {
	if seg.SymbolKind(sym) == facts.KindSection {
		return 1
	}
	return 0
}

// Search ranks live symbols for a free-text query with BM25 over name,
// signature, doc, package and file terms. A symbol matching more of the
// query's words ranks above one matching fewer; an exact name match ranks
// first. At most topK hits are returned.
func (v *View) Search(query string, topK int) []Scored {
	terms := queryTerms(query)
	if len(terms) == 0 || topK <= 0 {
		return nil
	}
	type hit struct {
		seg, sym int
		score    float64
		matched  int
		idf      float64 // idf mass of the matched terms
		corpus   int     // 0 code, 1 document sections
	}
	n, sumLen := 0, 0.0
	var cn [2]int // documents per corpus: code, sections
	var clen [2]float64
	for i := 0; i < v.sn.NumSegments(); i++ {
		seg := v.sn.Segment(i)
		docs, sl := seg.SearchStats()
		n += docs
		sumLen += sl
		ds := v.cache.sectionStats(v.sn, seg)
		cn[1] += ds.n
		clen[1] += ds.sumLen
	}
	if n == 0 {
		return nil
	}
	cn[0], clen[0] = max(0, n-cn[1]), max(0, sumLen-clen[1])
	var avg [2]float64
	for c := range avg {
		avg[c] = sumLen / float64(n)
		if cn[c] > 0 && clen[c] > 0 {
			avg[c] = clen[c] / float64(cn[c])
		}
	}
	// Document sections and code are scored as two corpora: prose repeats
	// the words code is named with, and must not lower their idf for code
	// (nor code for sections).
	hits := map[[2]int]*hit{}
	var idfs [2]map[string]float64
	idfs[0], idfs[1] = map[string]float64{}, map[string]float64{}
	var idfTotal [2]float64
	dfs := make(map[string]*[2]int, len(terms))
	rare := false
	for _, t := range terms {
		df := &[2]int{}
		dfs[t] = df
		for i := 0; i < v.sn.NumSegments(); i++ {
			seg := v.sn.Segment(i)
			lo, hi := seg.Postings(t)
			if hi-lo > maxPostings {
				df[0] += hi - lo
				continue
			}
			for k := lo; k < hi; k++ {
				sym, _ := seg.Posting(k)
				df[corpus(seg, sym)]++
			}
		}
		if all := df[0] + df[1]; all > 0 && !common(all, n) {
			rare = true
		}
	}
	for _, t := range terms {
		df := dfs[t]
		if df[0]+df[1] == 0 || (rare && common(df[0]+df[1], n)) {
			continue
		}
		var idf [2]float64
		for c := range idf {
			if df[c] > 0 {
				nc := max(cn[c], df[c])
				idf[c] = math.Log(1 + (float64(nc)-float64(df[c])+0.5)/(float64(df[c])+0.5))
				idfs[c][t] = idf[c]
				idfTotal[c] += idf[c]
			}
		}
		visited := 0
		for i := 0; i < v.sn.NumSegments(); i++ {
			seg := v.sn.Segment(i)
			lo, hi := seg.Postings(t)
			for k := lo; k < hi && visited < maxPostings; k++ {
				visited++
				sym, ptf := seg.Posting(k)
				if !v.sn.Live(i, seg.SymbolFile(sym)) {
					continue
				}
				c := corpus(seg, sym)
				if idf[c] == 0 {
					c = 0 // counted as code (a very common term)
				}
				tf := float64(ptf)
				dl := float64(seg.DocLen(sym))
				s := idf[c] * tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*dl/avg[c]))
				key := [2]int{i, sym}
				h := hits[key]
				if h == nil {
					h = &hit{seg: i, sym: sym, corpus: c}
					hits[key] = h
				}
				h.score += s
				h.matched++
				h.idf += idf[c]
			}
		}
	}
	// Rank on the raw records and decode only the best candidates: a common
	// term can hit thousands of symbols.
	lq := strings.ToLower(strings.TrimSpace(query))
	ranked := make([]*hit, 0, len(hits))
	for _, h := range hits {
		// Coverage by idf mass: matching the rare task words matters more
		// than matching many generic ones.
		coverage := 0.0
		if idfTotal[h.corpus] > 0 {
			coverage = min(1, h.idf/idfTotal[h.corpus])
		}
		h.score *= coverage * coverage
		seg := v.sn.Segment(h.seg)
		name := seg.SymbolName(h.sym)
		if idf, ok := idfs[h.corpus][stem(strings.ToLower(name))]; ok {
			h.score += idf // a task word is this declaration's own name
		}
		if strings.EqualFold(name, lq) || strings.EqualFold(qualifiedOf(seg.SymbolReceiver(h.sym), name), lq) {
			h.score += 1000
		}
		ranked = append(ranked, h)
	}
	slices.SortFunc(ranked, func(a, b *hit) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		if c := cmp.Compare(a.seg, b.seg); c != 0 {
			return c
		}
		return cmp.Compare(a.sym, b.sym)
	})
	// Keep every hit tied with the last kept score, so the final order by
	// location does not depend on which of the tied records were decoded.
	keep := min(len(ranked), topK)
	for keep < len(ranked) && keep > 0 && ranked[keep].score == ranked[keep-1].score {
		keep++
	}
	out := make([]Scored, 0, keep)
	for _, h := range ranked[:keep] {
		out = append(out, Scored{Symbol: v.symbol(h.seg, h.sym), Score: h.score, Matched: h.matched, Terms: len(terms)})
	}
	slices.SortFunc(out, func(a, b Scored) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		if c := cmp.Compare(a.File, b.File); c != 0 {
			return c
		}
		return cmp.Compare(a.Line, b.Line)
	})
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}

// Fuzzy returns declarations whose name contains substr (case-insensitive),
// ordered by name. When nothing contains it, names sharing the most trigrams
// with substr (closest by edit distance) are returned instead, as
// did-you-mean suggestions. At most 25 declarations are returned.
func (v *View) Fuzzy(substr string) []Symbol {
	q := strings.ToLower(strings.TrimSpace(substr))
	if q == "" {
		return nil
	}
	names := map[string]bool{}
	grams := terms.Trigrams(q)
	for i := 0; i < v.sn.NumSegments(); i++ {
		seg := v.sn.Segment(i)
		if len(grams) == 0 {
			for j := 0; j < seg.NumNames(); j++ {
				if name, lower := seg.Name(j); strings.Contains(lower, q) {
					names[strings.Clone(name)] = true
				}
			}
			continue
		}
		for _, j := range intersectNames(seg, grams) {
			if name, lower := seg.Name(j); strings.Contains(lower, q) {
				names[strings.Clone(name)] = true
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for n := range names {
		ordered = append(ordered, n)
	}
	slices.Sort(ordered)
	if len(ordered) == 0 && len(grams) > 0 {
		ordered = v.closestNames(q, grams)
	}
	var out []Symbol
	for _, name := range ordered {
		for _, s := range v.Symbols(name, "") {
			if s.Name != name {
				continue
			}
			out = append(out, s)
			if len(out) == fuzzyLimit {
				return out
			}
		}
	}
	return out
}

func intersectNames(seg *segment.Segment, grams []string) []int {
	var lists [][]int
	for _, g := range grams {
		l := seg.TrigramNames(g)
		if len(l) == 0 {
			return nil
		}
		lists = append(lists, l)
	}
	slices.SortFunc(lists, func(a, b []int) int { return cmp.Compare(len(a), len(b)) })
	out := slices.Clone(lists[0])
	for _, l := range lists[1:] {
		keep := out[:0]
		for _, x := range out {
			if _, ok := slices.BinarySearch(l, x); ok {
				keep = append(keep, x)
			}
		}
		out = keep
		if len(out) == 0 {
			break
		}
	}
	return out
}

// closestNames ranks names sharing at least a third of q's trigrams by edit
// distance and returns the best few.
func (v *View) closestNames(q string, grams []string) []string {
	type cand struct {
		name string
		dist int
	}
	shared := map[string]int{}
	for i := 0; i < v.sn.NumSegments(); i++ {
		seg := v.sn.Segment(i)
		counts := map[int]int{}
		for _, g := range grams {
			for _, j := range seg.TrigramNames(g) {
				counts[j]++
			}
		}
		for j, c := range counts {
			if c*3 >= len(grams) {
				name, _ := seg.Name(j)
				shared[strings.Clone(name)] = max(shared[name], c)
			}
		}
	}
	var cands []cand
	for name := range shared {
		cands = append(cands, cand{name, editDistance(q, strings.ToLower(name))})
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if c := cmp.Compare(a.dist, b.dist); c != 0 {
			return c
		}
		return cmp.Compare(a.name, b.name)
	})
	limit := max(1, len(q)/2)
	var out []string
	for _, c := range cands {
		if c.dist > limit || len(out) == 5 {
			break
		}
		out = append(out, c.name)
	}
	return out
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
