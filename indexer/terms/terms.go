// Package terms is the code-aware tokenizer shared by the segment writer
// (which stores search postings) and the query layer (which tokenizes
// queries the same way): words of letters, digits and '_', lower-cased,
// plus camelCase and snake_case parts, English stopwords dropped and common
// inflections stemmed.
package terms

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxQueryTerms bounds the distinct terms taken from one query.
const MaxQueryTerms = 32

// Field weights: the name dominates so a symbol whose name matches outranks
// one that merely mentions the term.
const (
	WeightName  = 3
	WeightOther = 1
)

// Add adds the terms of s to tf with weight w.
func Add(tf map[string]float32, s string, w float32) {
	for _, word := range Words(s) {
		lw := strings.ToLower(word)
		if stopwords[lw] {
			continue
		}
		tf[Stem(lw)] += w
		parts := Subwords(word)
		if len(parts) > 1 {
			for _, p := range parts {
				if lp := strings.ToLower(p); !stopwords[lp] {
					tf[Stem(lp)] += w / 2
				}
			}
		}
	}
}

// Query returns the sorted distinct terms of a search query.
func Query(q string) []string {
	tf := map[string]float32{}
	Add(tf, q, 1)
	out := make([]string, 0, len(tf))
	for t := range tf {
		out = append(out, t)
	}
	slices.Sort(out)
	if len(out) > MaxQueryTerms {
		out = out[:MaxQueryTerms]
	}
	return out
}

// Words splits s into runs of letters, digits and '_'.
func Words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' })
}

// Subwords splits an identifier at underscores and case changes:
// "parseHTTPRequest_v2" -> parse, HTTP, Request, v2.
func Subwords(word string) []string {
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

// Stem strips common English inflections so "edits", "edited" and
// "editing" meet "edit". It is deliberately crude: identifiers are matched
// exactly elsewhere; this only helps free-text recall.
func Stem(w string) string {
	if len(w) <= 4 {
		return w
	}
	if strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
		w = w[:len(w)-1] // plural first: "implementations" -> "implementation"
	}
	for _, suf := range []string{"ation", "ion", "ing", "ed", "er"} {
		if strings.HasSuffix(w, suf) && len(w)-len(suf) >= 4 {
			return w[:len(w)-len(suf)]
		}
	}
	return w
}

// Trigrams returns the rune trigrams of a lower-cased string, in order.
func Trigrams(s string) []string {
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

// stopwords are English function words that carry no code meaning.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true,
	"do": true, "does": true, "for": true, "from": true, "how": true, "i": true, "if": true, "in": true,
	"is": true, "it": true, "its": true, "of": true, "on": true, "or": true, "that": true, "the": true,
	"this": true, "to": true, "was": true, "what": true, "when": true, "where": true, "which": true,
	"why": true, "with": true, "we": true, "you": true, "can": true, "should": true, "before": true,
	"after": true, "into": true, "there": true, "then": true, "them": true, "our": true, "my": true,
}
