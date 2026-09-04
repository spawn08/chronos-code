package tui

import (
	"fmt"
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
		{name: "mcp command", input: "/mc", want: []string{"/mcp", "/mcp connect", "/compact"}},
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
		if got := inputCompletions(tt.input, agents, subagents, skills, nil, nil, nil); !reflect.DeepEqual(got, tt.want) {
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
		{input: "/copy v", want: []string{"/copy visible"}},
		{input: "/copy c", want: []string{"/copy code"}},
	}
	for _, tt := range tests {
		if got := inputCompletions(tt.input, nil, nil, nil, files, servers, nil); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("inputCompletions(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestInputCompletionsIncludesModelsAndThink(t *testing.T) {
	models := []string{"anthropic claude-sonnet-4-6", "openai gpt-4o"}
	tests := []struct {
		input string
		want  []string
	}{
		{input: "/model clau", want: []string{"/model anthropic claude-sonnet-4-6"}},
		{input: "/model gpt", want: []string{"/model openai gpt-4o"}},
		{input: "/think m", want: []string{"/think medium"}},
		{input: "/think of", want: []string{"/think off"}},
		{input: "/think hi", want: []string{"/think high"}},
	}
	for _, tt := range tests {
		if got := inputCompletions(tt.input, nil, nil, nil, nil, nil, models); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("inputCompletions(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestAppInputCompletionsSkipCatalogForPlainText(t *testing.T) {
	m := newTestAppModel(t)
	m.input.SetValue("please inspect the renderer")
	if got := m.inputCompletions(); got != nil {
		t.Fatalf("plain text completions = %v, want nil", got)
	}
	m.input.SetValue("/cop")
	if got := m.inputCompletions(); len(got) == 0 {
		t.Fatal("slash prefix produced no completions")
	}
}

func TestFileMentionCandidatesEmptyNeedleDoesNotScanAll(t *testing.T) {
	files := make([]string, 200)
	for i := range files {
		files[i] = fmt.Sprintf("pkg/file_%03d.go", i)
	}
	got := fileMentionCandidates("", files)
	if len(got) != maxCommandCompletions {
		t.Fatalf("empty needle completions = %d, want %d", len(got), maxCommandCompletions)
	}
	if got[0] != "@pkg/file_000.go" || got[maxCommandCompletions-1] != "@pkg/file_004.go" {
		t.Fatalf("empty needle completions = %v", got)
	}
}
