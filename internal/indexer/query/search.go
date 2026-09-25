package query

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/segment"
)

// BM25 parameters and field weights. The name field dominates so that a
// symbol whose name matches outranks one that merely mentions the term.
const (
	bm25K1       = 1.2
	bm25B        = 0.75
	weightName   = 3
	weightOther  = 1
	fuzzyLimit   = 25
	maxQueryTerm = 32
)

type posting struct {
	sym int32
	tf  float32
}

// segIndex is the search index of one immutable segment: BM25 postings over
// symbol name, signature, doc, package and file, plus a trigram index over
// distinct lower-cased symbol names for fuzzy lookup. Postings include
// records of files later replaced by overlays; queries filter liveness.
type segIndex struct {
	terms  map[string][]posting
	docLen []float32
	nDocs  int
	sumLen float64
	names  []string // distinct symbol names, sorted
	lower  []string // lower-cased names, parallel to names
	tri    map[string][]int32
}

func buildSegIndex(seg *segment.Segment) *segIndex {
	idx := &segIndex{terms: map[string][]posting{}, docLen: make([]float32, seg.NumSymbols()), tri: map[string][]int32{}}
	fileTerms := make(map[int]map[string]float32)
	tf := map[string]float32{}
	for k := 0; k < seg.NumSymbols(); k++ {
		if seg.SymbolKind(k) == facts.KindEmbed {
			continue
		}
		rec := seg.Symbol(k)
		clear(tf)
		addTerms(tf, rec.Name, weightName)
		addTerms(tf, rec.Receiver, weightOther)
		addTerms(tf, rec.Signature, weightOther)
		addTerms(tf, rec.Doc, weightOther)
		ft, ok := fileTerms[rec.File]
		if !ok {
			m := seg.FileMeta(rec.File)
			ft = map[string]float32{}
			addTerms(ft, m.Package, weightOther)
			addTerms(ft, m.Path, weightOther)
			fileTerms[rec.File] = ft
		}
		for t, n := range ft {
			tf[t] += n
		}
		var n float32
		for t, w := range tf {
			idx.terms[t] = append(idx.terms[t], posting{int32(k), w})
			n += w
		}
		idx.docLen[k] = n
		idx.nDocs++
		idx.sumLen += float64(n)
		if len(idx.names) == 0 || idx.names[len(idx.names)-1] != rec.Name {
			// Symbols are sorted by name, so distinct names arrive in order.
			idx.names = append(idx.names, rec.Name)
		}
	}
	idx.lower = make([]string, len(idx.names))
	for i, n := range idx.names {
		idx.lower[i] = strings.ToLower(n)
		for _, g := range trigrams(idx.lower[i]) {
			if l := idx.tri[g]; len(l) == 0 || l[len(l)-1] != int32(i) {
				idx.tri[g] = append(l, int32(i))
			}
		}
	}
	return idx
}

// addTerms adds the code-aware terms of s to tf with weight w: every word
// (letters, digits, underscore) lower-cased, plus its camelCase and
// snake_case parts when they differ.
func addTerms(tf map[string]float32, s string, w float32) {
	for _, word := range words(s) {
		lw := strings.ToLower(word)
		tf[lw] += w
		parts := subwords(word)
		if len(parts) > 1 {
			for _, p := range parts {
				tf[strings.ToLower(p)] += w / 2
			}
		}
	}
}

func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') })
}

// subwords splits an identifier at underscores and case changes:
// "parseHTTPRequest_v2" -> parse, HTTP, Request, v2.
func subwords(word string) []string {
	var out []string
	for _, part := range strings.Split(word, "_") {
		if part == "" {
			continue
		}
		rs := []rune(part)
		start := 0
		for i := 1; i < len(rs); i++ {
			prev, cur := rs[i-1], rs[i]
			next := rune(0)
			if i+1 < len(rs) {
				next = rs[i+1]
			}
			lowerToUpper := (unicode.IsLower(prev) || unicode.IsDigit(prev)) && unicode.IsUpper(cur)
			acronymEnd := unicode.IsUpper(prev) && unicode.IsUpper(cur) && next != 0 && unicode.IsLower(next)
			if lowerToUpper || acronymEnd {
				out = append(out, string(rs[start:i]))
				start = i
			}
		}
		out = append(out, string(rs[start:]))
	}
	return out
}

// queryTerms returns the distinct terms of a search query.
func queryTerms(q string) []string {
	tf := map[string]float32{}
	addTerms(tf, q, 1)
	out := make([]string, 0, len(tf))
	for t := range tf {
		out = append(out, t)
	}
	slices.Sort(out)
	if len(out) > maxQueryTerm {
		out = out[:maxQueryTerm]
	}
	return out
}

func trigrams(s string) []string {
	if utf8.RuneCountInString(s) < 3 {
		return nil
	}
	rs := []rune(s)
	out := make([]string, 0, len(rs)-2)
	for i := 0; i+3 <= len(rs); i++ {
		out = append(out, string(rs[i:i+3]))
	}
	return out
}

// Scored is a search hit.
type Scored struct {
	Symbol
	Score float64 // higher is better
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
	}
	n, sumLen := 0, 0.0
	idxs := make([]*segIndex, v.sn.NumSegments())
	for i := range idxs {
		idxs[i] = v.cache.segment(v.sn, i)
		n += idxs[i].nDocs
		sumLen += idxs[i].sumLen
	}
	if n == 0 {
		return nil
	}
	avg := sumLen / float64(n)
	hits := map[[2]int]*hit{}
	for _, t := range terms {
		df := 0
		for _, idx := range idxs {
			df += len(idx.terms[t])
		}
		if df == 0 {
			continue
		}
		idf := math.Log(1 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
		for i, idx := range idxs {
			seg := v.sn.Segment(i)
			for _, p := range idx.terms[t] {
				if !v.sn.Live(i, seg.SymbolFile(int(p.sym))) {
					continue
				}
				tf := float64(p.tf)
				dl := float64(idx.docLen[p.sym])
				s := idf * tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*dl/avg))
				key := [2]int{i, int(p.sym)}
				h := hits[key]
				if h == nil {
					h = &hit{seg: i, sym: int(p.sym)}
					hits[key] = h
				}
				h.score += s
				h.matched++
			}
		}
	}
	// Rank on the raw records and decode only the best candidates: a common
	// term can hit thousands of symbols.
	lq := strings.ToLower(strings.TrimSpace(query))
	ranked := make([]*hit, 0, len(hits))
	for _, h := range hits {
		coverage := float64(h.matched) / float64(len(terms))
		h.score *= coverage * coverage
		seg := v.sn.Segment(h.seg)
		name := seg.SymbolName(h.sym)
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
		out = append(out, Scored{Symbol: v.symbol(h.seg, h.sym), Score: h.score})
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
	grams := trigrams(q)
	for i := 0; i < v.sn.NumSegments(); i++ {
		idx := v.cache.segment(v.sn, i)
		if len(grams) == 0 {
			for j, l := range idx.lower {
				if strings.Contains(l, q) {
					names[idx.names[j]] = true
				}
			}
			continue
		}
		for _, j := range intersectPostings(idx.tri, grams) {
			if strings.Contains(idx.lower[j], q) {
				names[idx.names[j]] = true
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

func intersectPostings(tri map[string][]int32, grams []string) []int32 {
	var lists [][]int32
	for _, g := range grams {
		l := tri[g]
		if len(l) == 0 {
			return nil
		}
		lists = append(lists, l)
	}
	slices.SortFunc(lists, func(a, b []int32) int { return cmp.Compare(len(a), len(b)) })
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
		idx := v.cache.segment(v.sn, i)
		counts := map[int32]int{}
		for _, g := range grams {
			for _, j := range idx.tri[g] {
				counts[j]++
			}
		}
		for j, c := range counts {
			if c*3 >= len(grams) {
				shared[idx.names[j]] = max(shared[idx.names[j]], c)
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
