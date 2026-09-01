package incctx

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// matchThreshold is the minimum normalized token overlap required for a
// question to be considered a re-ask. Set high to avoid false positives.
const matchThreshold = 0.85

// answerPreviewLen is the maximum length of the short-circuited answer
// preview.
const answerPreviewLen = 150

// QuestionDedup tracks questions asked and answered within a session to
// detect re-asks. When the model asks a question sufficiently similar to
// one already answered, Check returns a short reference instead of
// requiring the exploration to be re-executed.
type QuestionDedup struct {
	mu      sync.Mutex
	entries []*answeredQuestion
}

type answeredQuestion struct {
	Question   string
	Normalized string
	Tokens     []string
	Answer     string
	Turn       int
}

// NewQuestionDedup returns an empty dedup tracker.
func NewQuestionDedup() *QuestionDedup {
	return &QuestionDedup{}
}

// Record stores a question-answer pair at the given turn number.
func (d *QuestionDedup) Record(question, answer string, turn int) {
	norm := normalize(question)
	tokens := tokenize(norm)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries = append(d.entries, &answeredQuestion{
		Question:   question,
		Normalized: norm,
		Tokens:     tokens,
		Answer:     answer,
		Turn:       turn,
	})
}

// Check returns a short-circuit answer if a sufficiently similar question
// was already asked and answered. Similarity is the fraction of the
// incoming question's tokens that appear in a recorded question (order-
// invariant, case-insensitive). Returns ("", false) when no match exceeds
// matchThreshold.
func (d *QuestionDedup) Check(question string) (string, bool) {
	tokens := tokenize(normalize(question))
	if len(tokens) == 0 {
		return "", false
	}
	incoming := toSet(tokens)

	d.mu.Lock()
	defer d.mu.Unlock()

	var best *answeredQuestion
	var bestScore float64

	for _, aq := range d.entries {
		if len(aq.Tokens) == 0 {
			continue
		}
		recorded := toSet(aq.Tokens)
		score := jaccardSimilarity(incoming, recorded)
		if score > bestScore {
			bestScore = score
			best = aq
		}
	}

	if bestScore < matchThreshold || best == nil {
		return "", false
	}

	preview := best.Answer
	if len(preview) > answerPreviewLen {
		preview = preview[:answerPreviewLen] + "..."
	}
	return fmt.Sprintf("Already determined at turn %d: %s", best.Turn, preview), true
}

// normalize lowercases text, strips punctuation, tokenizes, sorts, and
// rejoins — producing an order-invariant canonical form.
func normalize(text string) string {
	tokens := tokenize(strings.ToLower(text))
	sort.Strings(tokens)
	return strings.Join(tokens, " ")
}

// tokenize splits text into lowercase alphabetic/numeric tokens of length
// >1, stripping punctuation. Matches the tokenizer in memory/recall.go.
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

func toSet(tokens []string) map[string]bool {
	s := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		s[t] = true
	}
	return s
}

// jaccardSimilarity returns |A∩B| / |A∪B|.
func jaccardSimilarity(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	intersection := 0
	for k := range a {
		if b[k] {
			intersection++
		}
	}
	union := len(a)
	for k := range b {
		if !a[k] {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}
