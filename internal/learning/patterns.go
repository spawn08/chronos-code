package learning

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/cespare/xxhash/v2"
)

// MinimumCandidateCount is the number of matching observations required
// before a pattern can become a candidate.
const MinimumCandidateCount = 3

// SessionSegment is one user request and the ordered activity that follows it.
type SessionSegment struct {
	SessionID string
	RepoPath  string
	Trigger   Turn
	Turns     []Turn
	ToolCalls []ToolCall
	Outcome   *Outcome
}

// PatternCandidate is an exact normalized-trigger group that passed the
// minimum observation and success-rate thresholds.
type PatternCandidate struct {
	ID              int64
	RepoPath        string
	TriggerHash     string
	SolutionSummary string
	ToolSequence    []string
	SuccessCount    int64
	FailCount       int64
	LastUsedAt      time.Time
}

// NormalizeTrigger makes case, punctuation, and whitespace differences
// irrelevant without applying fuzzy or model-based matching.
func NormalizeTrigger(trigger string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(trigger) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

type candidateGroup struct {
	candidate      PatternCandidate
	sequenceCounts map[string]int
	sequences      map[string][]string
	solutionAt     time.Time
	solutionSet    bool
}

// ClusterCandidates groups segments by repository and exact normalized
// trigger hash, returning only groups above the configured thresholds.
func ClusterCandidates(segments []SessionSegment, minimumCount int) []PatternCandidate {
	groups := make(map[string]*candidateGroup)
	for _, segment := range segments {
		if segment.Outcome == nil {
			continue
		}
		normalized := NormalizeTrigger(segment.Trigger.Content)
		if normalized == "" {
			continue
		}
		hash := triggerHash(normalized)
		key := segment.RepoPath + "\x00" + hash
		group := groups[key]
		if group == nil {
			group = &candidateGroup{
				candidate:      PatternCandidate{RepoPath: segment.RepoPath, TriggerHash: hash},
				sequenceCounts: make(map[string]int),
				sequences:      make(map[string][]string),
			}
			groups[key] = group
		}

		if segment.Outcome.Kind == "accepted" {
			group.candidate.SuccessCount++
			sequence := toolNames(segment.ToolCalls)
			encoded, _ := json.Marshal(sequence)
			sequenceKey := string(encoded)
			group.sequenceCounts[sequenceKey]++
			group.sequences[sequenceKey] = sequence

			summary := finalAssistantContent(segment.Turns)
			if !group.solutionSet || segment.Outcome.Timestamp.After(group.solutionAt) ||
				(segment.Outcome.Timestamp.Equal(group.solutionAt) && summary < group.candidate.SolutionSummary) {
				group.solutionAt = segment.Outcome.Timestamp
				group.solutionSet = true
				group.candidate.SolutionSummary = summary
			}
		} else {
			group.candidate.FailCount++
		}
		if segment.Outcome.Timestamp.After(group.candidate.LastUsedAt) {
			group.candidate.LastUsedAt = segment.Outcome.Timestamp
		}
	}

	candidates := make([]PatternCandidate, 0, len(groups))
	for _, group := range groups {
		total := group.candidate.SuccessCount + group.candidate.FailCount
		if total < int64(minimumCount) || group.candidate.SuccessCount*100 <= total*70 {
			continue
		}
		group.candidate.ToolSequence = mostFrequentSequence(group.sequenceCounts, group.sequences)
		candidates = append(candidates, group.candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].RepoPath != candidates[j].RepoPath {
			return candidates[i].RepoPath < candidates[j].RepoPath
		}
		return candidates[i].TriggerHash < candidates[j].TriggerHash
	})
	return candidates
}

func triggerHash(normalized string) string {
	return fmt.Sprintf("%016x", xxhash.Sum64String(normalized))
}

func toolNames(calls []ToolCall) []string {
	names := make([]string, len(calls))
	for i, call := range calls {
		names[i] = call.Name
	}
	return names
}

func finalAssistantContent(turns []Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" {
			return turns[i].Content
		}
	}
	return ""
}

func mostFrequentSequence(counts map[string]int, sequences map[string][]string) []string {
	bestKey := ""
	bestCount := -1
	for key, count := range counts {
		if count > bestCount || (count == bestCount && key < bestKey) {
			bestKey = key
			bestCount = count
		}
	}
	return sequences[bestKey]
}
