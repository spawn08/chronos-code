package skills

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/spawn08/chronos/engine/model"
)

// DefaultTopK is the maximum number of skills injected per turn (ROADMAP.md
// §5.1: "Top-K (default 3) skills are injected").
const DefaultTopK = 3

// TokenBudget is the total token ceiling across every selected skill's
// rendered block (ROADMAP.md §5.1: "Token budget: skills capped at 8k
// tokens total per turn; oldest drops first").
const TokenBudget = 8000

// bm25K1 and bm25B are the standard Okapi BM25 tuning constants.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// Select scores every skill in all against message using BM25 over each
// skill's triggers+description+name (ROADMAP.md §5.1: "a lightweight
// classifier (BM25 over triggers + description) against the current user
// message"), keeps the topK highest-scoring skills with a positive score,
// then trims from the lowest-scored end of that set until the rendered
// total fits within TokenBudget tokens (counted via modelID's tokenizer).
// An empty or all-zero-score result returns nil, not an error — "nothing
// relevant enough to inject" is the common case, not a failure.
func Select(message string, all []*Skill, topK int, modelID string) []*Skill {
	if topK <= 0 {
		topK = DefaultTopK
	}
	scored := bm25Rank(message, all)
	if len(scored) > topK {
		scored = scored[:topK]
	}

	counter := model.NewTokenCounter(modelID)
	selected := make([]*Skill, len(scored))
	for i, sc := range scored {
		selected[i] = sc.skill
	}
	for len(selected) > 0 && counter.CountString(Render(selected)) > TokenBudget {
		selected = selected[:len(selected)-1] // drop the lowest-scored (last) skill first.
	}
	return selected
}

// Render formats selected skills as ROADMAP.md §5.1's
// `<skill name="…">…</skill>` blocks, in the order given.
func Render(selected []*Skill) string {
	if len(selected) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range selected {
		fmt.Fprintf(&b, "<skill name=%q>\n%s\n</skill>\n", s.Name, s.Body)
	}
	return strings.TrimSpace(b.String())
}

type scoredSkill struct {
	skill *Skill
	score float64
}

// bm25Rank scores every skill in all against message's tokens via Okapi
// BM25, filters out non-positive scores, and returns them sorted by score
// descending (ties broken by declaration order, for determinism).
func bm25Rank(message string, all []*Skill) []scoredSkill {
	queryTokens := tokenize(message)
	if len(queryTokens) == 0 || len(all) == 0 {
		return nil
	}

	docTokens := make([][]string, len(all))
	var totalLen int
	df := make(map[string]int) // term -> number of skills whose text contains it
	for i, s := range all {
		docTokens[i] = tokenize(s.text())
		totalLen += len(docTokens[i])
		seen := make(map[string]bool, len(docTokens[i]))
		for _, t := range docTokens[i] {
			if !seen[t] {
				seen[t] = true
				df[t]++
			}
		}
	}
	n := float64(len(all))
	avgDocLen := float64(totalLen) / n

	idf := func(term string) float64 {
		d := float64(df[term])
		return math.Log(1 + (n-d+0.5)/(d+0.5))
	}

	var out []scoredSkill
	for i, s := range all {
		tf := termFreqs(docTokens[i])
		docLen := float64(len(docTokens[i]))
		var score float64
		for _, qt := range queryTokens {
			f := float64(tf[qt])
			if f == 0 {
				continue
			}
			score += idf(qt) * (f * (bm25K1 + 1)) / (f + bm25K1*(1-bm25B+bm25B*docLen/avgDocLen))
		}
		if score > 0 {
			out = append(out, scoredSkill{skill: s, score: score})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

// tokenize and termFreqs mirror internal/memory/recall.go's TF helpers:
// lowercase alphanumeric tokens, stripped of punctuation and 1-char noise.
func tokenize(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(words))
	for _, w := range words {
		if len(w) > 1 {
			out = append(out, w)
		}
	}
	return out
}

func termFreqs(tokens []string) map[string]int {
	tf := make(map[string]int, len(tokens))
	for _, t := range tokens {
		tf[t]++
	}
	return tf
}
