package tui

import (
	"sort"
	"strings"
)

const maxCommandCompletions = 5

// commandCompletions returns fuzzy matches while the user is typing a slash
// command. Arguments disable completion so Tab remains available to the
// textarea once a command has been chosen.
func commandCompletions(input string) []string {
	if !strings.HasPrefix(input, "/") || strings.ContainsAny(input, " \t\n") {
		return nil
	}

	type match struct {
		command string
		score   int
	}
	var matches []match
	query := strings.ToLower(input)
	for _, command := range paletteCommands {
		candidate := strings.ToLower(command)
		score := fuzzyCommandScore(candidate, query)
		if score >= 0 {
			matches = append(matches, match{command: command, score: score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score < matches[j].score
		}
		return len(matches[i].command) < len(matches[j].command)
	})

	limit := len(matches)
	if limit > maxCommandCompletions {
		limit = maxCommandCompletions
	}
	result := make([]string, limit)
	for i := 0; i < limit; i++ {
		result[i] = matches[i].command
	}
	return result
}

func fuzzyCommandScore(candidate, query string) int {
	if candidate == query {
		return 0
	}
	if strings.HasPrefix(candidate, query) {
		return 1
	}

	queryIdx := 0
	gaps := 0
	for candidateIdx := 0; candidateIdx < len(candidate) && queryIdx < len(query); candidateIdx++ {
		if candidate[candidateIdx] == query[queryIdx] {
			queryIdx++
		} else if queryIdx > 0 {
			gaps++
		}
	}
	if queryIdx != len(query) {
		return -1
	}
	return 2 + gaps
}
