package tui

import (
	"sort"
	"strings"
)

const maxCommandCompletions = 5

// inputCompletions returns fuzzy matches for commands, explicit skill
// invocations, agent mentions, and agent-valued command arguments.
func inputCompletions(input string, agents, subagents, skillNames []string) []string {
	var candidates []string
	switch {
	case strings.HasPrefix(input, "@") && !strings.ContainsAny(input, " \t\n"):
		for _, name := range agents {
			candidates = append(candidates, "@"+name+" ")
		}
	case strings.HasPrefix(input, "/agent ") && !strings.ContainsAny(strings.TrimPrefix(input, "/agent "), " \t\n"):
		for _, name := range agents {
			candidates = append(candidates, "/agent "+name)
		}
	case strings.HasPrefix(input, "/subagent ") && !strings.ContainsAny(strings.TrimPrefix(input, "/subagent "), " \t\n"):
		for _, name := range subagents {
			candidates = append(candidates, "/subagent "+name+" ")
		}
	case strings.HasPrefix(input, "/") && !strings.ContainsAny(input, " \t\n"):
		candidates = append(candidates, paletteCommands...)
		for _, name := range skillNames {
			candidates = append(candidates, "/"+name)
		}
	default:
		return nil
	}

	type match struct {
		command string
		score   int
	}
	var matches []match
	query := strings.ToLower(input)
	for _, command := range candidates {
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

func (m *appModel) inputCompletions() []string {
	if m.orch == nil {
		return inputCompletions(m.input.Value(), nil, nil, nil)
	}
	skillNames := make([]string, 0, len(m.orch.ListSkills()))
	for _, skill := range m.orch.ListSkills() {
		skillNames = append(skillNames, skill.Name)
	}
	return inputCompletions(m.input.Value(), m.orch.ListAgents(), m.orch.ListSubagents(), skillNames)
}

func commandCompletions(input string) []string {
	if !strings.HasPrefix(input, "/") || strings.ContainsAny(input, " \t\n") {
		return nil
	}
	return inputCompletions(input, nil, nil, nil)
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
