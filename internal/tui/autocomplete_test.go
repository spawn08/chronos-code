package tui

import (
	"reflect"
	"testing"
)

func TestCommandCompletions(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "empty", input: "", want: nil},
		{name: "plain text", input: "help", want: nil},
		{name: "arguments", input: "/agent coder", want: nil},
		{name: "prefix orders shortest first", input: "/ag", want: []string{"/agent", "/agents", "/usage", "/subagent"}},
		{name: "exact first", input: "/agent", want: []string{"/agent", "/agents", "/subagent"}},
		{name: "mcp command", input: "/mc", want: []string{"/mcp", "/mcp connect"}},
		{name: "fuzzy subsequence", input: "/mdl", want: []string{"/model"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commandCompletions(tt.input); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("commandCompletions(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestInputCompletionsIncludesSkillsAndAgents(t *testing.T) {
	agents := []string{"researcher", "reviewer"}
	subagents := []string{"researcher", "reviewer"}
	skills := []string{"code-review", "test-writer"}
	tests := []struct {
		input string
		want  []string
	}{
		{input: "/code-r", want: []string{"/code-review"}},
		{input: "@res", want: []string{"@researcher "}},
		{input: "/agent rev", want: []string{"/agent reviewer"}},
		{input: "/subagent res", want: []string{"/subagent researcher "}},
	}
	for _, tt := range tests {
		if got := inputCompletions(tt.input, agents, subagents, skills, nil, nil); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("inputCompletions(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestInputCompletionsIncludesFilesAndMCPServers(t *testing.T) {
	files := []string{"internal/tui/app.go", "internal/tui/mentions.go", "README.md"}
	servers := []string{"filesystem", "github"}
	tests := []struct {
		input string
		want  []string
	}{
		{input: "@app.go", want: []string{"@internal/tui/app.go"}},
		{input: "look at @README", want: []string{"@README.md"}},
		{input: "/mcp connect g", want: []string{"/mcp connect github"}},
	}
	for _, tt := range tests {
		if got := inputCompletions(tt.input, nil, nil, nil, files, servers); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("inputCompletions(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
