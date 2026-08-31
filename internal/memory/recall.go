package memory

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// ScoredRecord pairs a Record with a relevance score (higher is better).
type ScoredRecord struct {
	Record Record
	Score  float64
}

// minScore filters out noise: records scoring below this threshold are
// excluded from Recall results.
const minScore = 0.05

// Recall returns records ranked by term-frequency relevance to query. It is
// deterministic and zero-cost — no external API calls, no embeddings. An
// empty query returns all records (score 1.0 each). An empty store returns
// nil. Results are sorted by score descending and limited to maxResults
// (0 or negative means unlimited).
func (s *Store) Recall(query string, maxResults int) ([]ScoredRecord, error) {
	all, err := s.List("")
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}

	queryLower := strings.ToLower(strings.TrimSpace(query))
	if queryLower == "" {
		out := make([]ScoredRecord, len(all))
		for i, r := range all {
			out[i] = ScoredRecord{Record: r, Score: 1.0}
		}
		if maxResults > 0 && len(out) > maxResults {
			out = out[:maxResults]
		}
		return out, nil
	}

	queryTokens := tokenize(queryLower)
	if len(queryTokens) == 0 {
		return nil, nil
	}
	queryTF := termFreqs(queryTokens)

	var scored []ScoredRecord
	for _, rec := range all {
		score := scoreRecord(queryLower, queryTF, len(queryTokens), rec)
		if score >= minScore {
			scored = append(scored, ScoredRecord{Record: rec, Score: score})
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Record.CreatedAt.After(scored[j].Record.CreatedAt)
	})

	if maxResults > 0 && len(scored) > maxResults {
		scored = scored[:maxResults]
	}
	return scored, nil
}

func scoreRecord(queryLower string, queryTF map[string]int, queryLen int, rec Record) float64 {
	docLower := strings.ToLower(rec.Content)
	docTokens := tokenize(docLower)
	if len(docTokens) == 0 {
		return 0
	}
	docTF := termFreqs(docTokens)

	var overlap float64
	for term, qCount := range queryTF {
		if dCount, ok := docTF[term]; ok {
			overlap += math.Min(float64(qCount), float64(dCount))
		}
	}

	if overlap == 0 {
		return 0
	}

	// Normalize by geometric mean of query and doc lengths to avoid bias
	// toward very short or very long documents.
	norm := math.Sqrt(float64(queryLen) * float64(len(docTokens)))
	score := overlap / norm

	// Exact phrase match bonus: if the full query appears as a substring,
	// boost the score by 50%.
	if strings.Contains(docLower, queryLower) {
		score *= 1.5
	}

	// Category match bonus: if the query mentions a category name and the
	// record belongs to that category, boost by 10%.
	catLower := strings.ToLower(string(rec.Category))
	if strings.Contains(queryLower, catLower) {
		score *= 1.1
	}

	return score
}

// tokenize splits text into lowercase alphabetic/numeric tokens, stripping
// punctuation and short (1-char) tokens.
func tokenize(text string) []string {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(words))
	for _, w := range words {
		w = strings.ToLower(w)
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
