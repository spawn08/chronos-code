package learning

import (
	"context"
	"errors"
	"testing"

	"github.com/spawn08/chronos/engine/model"
)

// fakeProvider is a test double for model.Provider that returns a canned
// chat response without calling any real model API.
type fakeProvider struct {
	content string
	err     error
	calls   int
}

func (f *fakeProvider) Chat(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &model.ChatResponse{Content: f.content}, nil
}

func (f *fakeProvider) StreamChat(ctx context.Context, req *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeProvider) Name() string  { return "fake" }
func (f *fakeProvider) Model() string { return "fake-model" }

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := Parse([]byte(sampleLearningYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return cfg
}

const agentSuggestionYAML = `
kind: agent
agent_id: search-first
title: A leaner search-first agent
rationale: file_grep is followed by file_read in 80% of tool sequences.
confidence: 0.8
yaml: |
  id: search-first
  name: Search-First Agent
  model:
    provider: anthropic
    model: claude-haiku-4-5
`

func TestDistiller_Suggest_AgentKind(t *testing.T) {
	provider := &fakeProvider{content: agentSuggestionYAML}
	d := NewDistiller(provider, testConfig(t))

	report := &Report{AgentID: "coder", Sessions: []string{"sess_1"}, TotalSpans: 4, ToolCounts: map[string]int{"file_grep": 3}}
	sug, err := d.Suggest(context.Background(), report)
	if err != nil {
		t.Fatalf("Suggest() error = %v", err)
	}
	if sug == nil {
		t.Fatal("Suggest() = nil, want a Suggestion")
	}
	if sug.Kind != "agent" || sug.AgentID != "search-first" {
		t.Errorf("Suggest() = %+v, want kind=agent agent_id=search-first", sug)
	}
	if sug.Confidence != 0.8 {
		t.Errorf("Confidence = %v, want 0.8", sug.Confidence)
	}
	if sug.SourceAgentID != "coder" {
		t.Errorf("SourceAgentID = %q, want coder", sug.SourceAgentID)
	}
	if sug.ID == "" {
		t.Error("ID is empty")
	}
	if provider.calls != 1 {
		t.Errorf("provider.calls = %d, want 1", provider.calls)
	}
}

func TestDistiller_Suggest_NoneKindReturnsNil(t *testing.T) {
	provider := &fakeProvider{content: "kind: none\nrationale: nothing recurring\nconfidence: 0.0\nyaml: \"\"\n"}
	d := NewDistiller(provider, testConfig(t))

	sug, err := d.Suggest(context.Background(), &Report{AgentID: "coder"})
	if err != nil {
		t.Fatalf("Suggest() error = %v", err)
	}
	if sug != nil {
		t.Errorf("Suggest() = %+v, want nil for kind=none", sug)
	}
}

func TestDistiller_Suggest_StripsCodeFence(t *testing.T) {
	fenced := "```yaml\n" + agentSuggestionYAML + "```"
	provider := &fakeProvider{content: fenced}
	d := NewDistiller(provider, testConfig(t))

	sug, err := d.Suggest(context.Background(), &Report{AgentID: "coder"})
	if err != nil {
		t.Fatalf("Suggest() error = %v", err)
	}
	if sug == nil || sug.AgentID != "search-first" {
		t.Errorf("Suggest() = %+v, want agent_id=search-first despite code fence", sug)
	}
}

func TestDistiller_Suggest_ProviderError(t *testing.T) {
	provider := &fakeProvider{err: errors.New("no api key")}
	d := NewDistiller(provider, testConfig(t))

	if _, err := d.Suggest(context.Background(), &Report{AgentID: "coder"}); err == nil {
		t.Error("Suggest() error = nil, want an error when the provider fails")
	}
}

func TestDistiller_Suggest_MalformedResponse(t *testing.T) {
	provider := &fakeProvider{content: "not: [valid"}
	d := NewDistiller(provider, testConfig(t))

	if _, err := d.Suggest(context.Background(), &Report{AgentID: "coder"}); err == nil {
		t.Error("Suggest() error = nil, want an error for malformed YAML reply")
	}
}
