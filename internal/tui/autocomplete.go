package tui

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/spawn08/chronos-code/internal/modelinfo"
)

const maxCommandCompletions = 5

var mcpSubcommands = []string{"/mcp connect"}

// inputCompletions returns fuzzy matches for commands, explicit skill
// invocations, agent mentions, file mentions, and command arguments.
func inputCompletions(input string, agents, subagents, skillNames, files, mcpServers, models []string) []string {
	_, query := completionSpan(input)
	if strings.HasPrefix(query, "@") {
		return atCompletions(query, agents, files)
	}

	var candidates []string
	switch {
	case strings.HasPrefix(input, "/think") && (input == "/think" || strings.HasPrefix(input, "/think ")):
		rest := strings.TrimSpace(strings.TrimPrefix(input, "/think"))
		if strings.ContainsAny(rest, " \t\n") {
			return nil
		}
		for _, name := range []string{"off", "low", "medium", "high"} {
			candidates = append(candidates, "/think "+name)
		}
		candidates = append(candidates, "/think")
	case strings.HasPrefix(input, "/model "):
		for _, name := range models {
			candidates = append(candidates, "/model "+name)
		}
		if len(candidates) == 0 {
			return nil
		}
	case strings.HasPrefix(input, "/learn") && (input == "/learn" || strings.HasPrefix(input, "/learn ")):
		rest := strings.TrimSpace(strings.TrimPrefix(input, "/learn"))
		if strings.ContainsAny(rest, " \t\n") {
			return nil
		}
		for _, name := range []string{"list", "accept", "reject"} {
			candidates = append(candidates, "/learn "+name)
		}
		candidates = append(candidates, "/learn")
	case strings.HasPrefix(input, "/copy") && (input == "/copy" || strings.HasPrefix(input, "/copy ")):
		rest := strings.TrimSpace(strings.TrimPrefix(input, "/copy"))
		if strings.ContainsAny(rest, " \t\n") {
			return nil
		}
		for _, name := range []string{"last", "visible", "all", "code"} {
			candidates = append(candidates, "/copy "+name)
		}
		candidates = append(candidates, "/copy")
	case strings.HasPrefix(input, "/mcp connect") && (input == "/mcp connect" || strings.HasPrefix(input, "/mcp connect ")):
		rest := strings.TrimSpace(strings.TrimPrefix(input, "/mcp connect"))
		if strings.ContainsAny(rest, " \t\n") {
			return nil
		}
		for _, name := range mcpServers {
			candidates = append(candidates, "/mcp connect "+name)
		}
		if len(candidates) == 0 {
			candidates = append(candidates, "/mcp connect")
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
		candidates = append(candidates, mcpSubcommands...)
		for _, name := range skillNames {
			candidates = append(candidates, "/"+name)
		}
	default:
		return nil
	}
	return rankCompletions(candidates, strings.ToLower(input))
}

func atCompletions(query string, agents, files []string) []string {
	needle := strings.TrimPrefix(query, "@")
	var out []string
	if !strings.ContainsAny(needle, "/.") {
		var agentCandidates []string
		for _, name := range agents {
			agentCandidates = append(agentCandidates, "@"+name+" ")
		}
		out = append(out, rankCompletions(agentCandidates, strings.ToLower(query))...)
	}
	for _, file := range fileMentionCandidates(needle, files) {
		duplicate := false
		for _, existing := range out {
			if existing == file {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, file)
		}
	}
	if len(out) > maxCommandCompletions {
		out = out[:maxCommandCompletions]
	}
	return out
}

func rankCompletions(candidates []string, needle string) []string {
	type match struct {
		command string
		score   int
	}
	var matches []match
	for _, command := range candidates {
		score := fuzzyCommandScore(strings.ToLower(command), needle)
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

func fileMentionCandidates(needle string, files []string) []string {
	if len(files) == 0 {
		return nil
	}
	needle = strings.ToLower(needle)
	type match struct {
		path  string
		score int
	}
	var matches []match
	for _, file := range files {
		path := filepath.ToSlash(file)
		score := fileMentionScore(path, needle)
		if score >= 0 {
			matches = append(matches, match{path: path, score: score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score < matches[j].score
		}
		if len(matches[i].path) != len(matches[j].path) {
			return len(matches[i].path) < len(matches[j].path)
		}
		return matches[i].path < matches[j].path
	})
	limit := len(matches)
	if limit > maxCommandCompletions {
		limit = maxCommandCompletions
	}
	out := make([]string, limit)
	for i := 0; i < limit; i++ {
		out[i] = "@" + matches[i].path
	}
	return out
}

func fileMentionScore(path, needle string) int {
	if needle == "" {
		return 3
	}
	lower := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(path))
	if base == needle || lower == needle {
		return 0
	}
	if strings.HasPrefix(base, needle) || strings.HasPrefix(lower, needle) {
		return 1
	}
	return fuzzyCommandScore(lower, needle)
}

func (m *appModel) inputCompletions() []string {
	value := m.input.Value()
	if m.completionCached && m.completionCacheKey == value {
		return m.completionCache
	}
	out := m.computeInputCompletions(value)
	m.completionCacheKey = value
	m.completionCache = out
	m.completionCached = true
	return out
}

func (m *appModel) computeInputCompletions(value string) []string {
	if m.orch == nil {
		return inputCompletions(value, nil, nil, nil, nil, nil, nil)
	}
	_, query := completionSpan(value)
	at := strings.HasPrefix(query, "@")
	slash := strings.HasPrefix(value, "/")
	if !at && !slash {
		return nil
	}

	var agents, subagents, skillNames, files, mcpServers, models []string
	if at || strings.HasPrefix(value, "/agent ") || strings.HasPrefix(value, "/subagent ") {
		agents = m.orch.ListAgents()
	}
	if strings.HasPrefix(value, "/subagent ") {
		subagents = m.orch.ListSubagents()
	}
	if slash && !strings.ContainsAny(value, " \t\n") {
		for _, skill := range m.orch.ListSkills() {
			skillNames = append(skillNames, skill.Name)
		}
	}
	if at {
		if ws := m.orch.Workspace(); ws != nil {
			files = ws.Files
		}
	}
	if strings.HasPrefix(value, "/mcp") {
		for _, status := range m.orch.MCPStatuses() {
			mcpServers = append(mcpServers, status.Name)
		}
	}
	if strings.HasPrefix(value, "/model ") {
		models = m.modelCompletionValues()
	}
	return inputCompletions(value, agents, subagents, skillNames, files, mcpServers, models)
}

func (m *appModel) modelCompletionValues() []string {
	list := modelinfo.All()
	if m.orch != nil {
		authorized := m.orch.AuthorizedProviders(m.ctx, distinctProviders(list))
		if len(authorized) > 0 {
			list = filterByProviders(list, authorized)
		}
	}
	out := make([]string, 0, len(list))
	for _, info := range list {
		out = append(out, info.Provider+" "+info.Model)
	}
	return out
}

func commandCompletions(input string) []string {
	if !strings.HasPrefix(input, "/") || strings.ContainsAny(input, " \t\n") {
		return nil
	}
	return inputCompletions(input, nil, nil, nil, nil, nil, nil)
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
