package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spawn08/chronos/sdk/agent"
	"gopkg.in/yaml.v3"
)

func TestHooksUnmarshalPreservesDeclarationOrder(t *testing.T) {
	var cfg Config
	body := []byte(`hooks:
  pre_tool_call:
    - name: first
      command: echo first
      timeout_ms: 1000
    - name: second
      command: echo second
      timeout_ms: 2000
  post_tool_call:
    - name: after
      command: echo after
      timeout_ms: 3000
  user_prompt_submit:
    - name: prompt
      command: echo prompt
      timeout_ms: 4000
`)
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got, want := cfg.Hooks.PreToolCall, []HookDef{
		{Name: "first", Command: "echo first", TimeoutMs: 1000},
		{Name: "second", Command: "echo second", TimeoutMs: 2000},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("PreToolCall = %#v, want %#v", got, want)
	}
	if got := cfg.Hooks.PostToolCall; len(got) != 1 || got[0].Name != "after" {
		t.Errorf("PostToolCall = %#v, want after hook", got)
	}
	if got := cfg.Hooks.UserPromptSubmit; len(got) != 1 || got[0].Name != "prompt" {
		t.Errorf("UserPromptSubmit = %#v, want prompt hook", got)
	}
}

func TestMergeHooksReplacesOnlyExplicitPoints(t *testing.T) {
	base := HooksConfig{
		PreToolCall:      []HookDef{{Name: "base-pre"}},
		PostToolCall:     []HookDef{{Name: "base-post"}},
		UserPromptSubmit: []HookDef{{Name: "base-prompt"}},
	}
	var overlay Config
	if err := yaml.Unmarshal([]byte(`hooks:
  pre_tool_call:
    - name: overlay-pre
      command: echo overlay
      timeout_ms: 1000
  post_tool_call: []
`), &overlay); err != nil {
		t.Fatalf("unmarshal overlay: %v", err)
	}

	mergeHooks(&base, overlay.Hooks)

	if got := base.PreToolCall; len(got) != 1 || got[0].Name != "overlay-pre" {
		t.Errorf("PreToolCall = %#v, want overlay hook", got)
	}
	if base.PostToolCall == nil || len(base.PostToolCall) != 0 {
		t.Errorf("PostToolCall = %#v, want explicit empty replacement", base.PostToolCall)
	}
	if got := base.UserPromptSubmit; len(got) != 1 || got[0].Name != "base-prompt" {
		t.Errorf("UserPromptSubmit = %#v, want inherited hook", got)
	}
}

func TestHooksValidation(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "empty name", body: hookYAML("", "echo ok", 1), wantErr: "hooks.pre_tool_call[0].name"},
		{name: "invalid name", body: hookYAML("bad name", "echo ok", 1), wantErr: "hooks.pre_tool_call[0].name"},
		{name: "empty command", body: hookYAML("valid", "  ", 1), wantErr: "hooks.pre_tool_call[0].command"},
		{name: "zero timeout", body: hookYAML("valid", "echo ok", 0), wantErr: "hooks.pre_tool_call[0].timeout_ms"},
		{name: "excessive timeout", body: hookYAML("valid", "echo ok", maxHookTimeoutMs+1), wantErr: "hooks.pre_tool_call[0].timeout_ms"},
		{
			name: "duplicate name",
			body: `hooks:
  post_tool_call:
    - name: duplicate
      command: echo one
      timeout_ms: 1
    - name: duplicate
      command: echo two
      timeout_ms: 1
`,
			wantErr: "hooks.post_tool_call[1].name: duplicate name",
		},
		{name: "unsupported point", body: "hooks:\n  before_agent: []\n", wantErr: "hooks.before_agent: unsupported hook point"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			err := yaml.Unmarshal([]byte(tt.body), &cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("unmarshal error = %v, want field context %q", err, tt.wantErr)
			}
		})
	}
}

func TestHooksAbsentIsCompatible(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("router:\n  enabled: true\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(cfg.Hooks, HooksConfig{}) {
		t.Errorf("Hooks = %#v, want zero value", cfg.Hooks)
	}

	base := HooksConfig{PreToolCall: []HookDef{{Name: "inherited"}}}
	mergeHooks(&base, cfg.Hooks)
	if got := base.PreToolCall; len(got) != 1 || got[0].Name != "inherited" {
		t.Errorf("PreToolCall = %#v, want inherited hook", got)
	}
}

func hookYAML(name, command string, timeout int) string {
	return "hooks:\n  pre_tool_call:\n    - name: " + name + "\n      command: '" + command + "'\n      timeout_ms: " + fmt.Sprint(timeout) + "\n"
}

// TestProvidersUnmarshal_BaseURL covers AC-2.2's config-parsing side: a
// providers.<name>.base_url entry in config.yaml must land in
// Config.Providers.
func TestProvidersUnmarshal_BaseURL(t *testing.T) {
	var cfg Config
	body := []byte("providers:\n  anthropic:\n    base_url: \"https://internal-gateway.corp.com/claude\"\n")
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, want := cfg.Providers["anthropic"].BaseURL, "https://internal-gateway.corp.com/claude"; got != want {
		t.Errorf("Providers[\"anthropic\"].BaseURL = %q, want %q", got, want)
	}
}

// TestProvidersUnmarshal_MissingSection covers the contract invariant that
// a config.yaml without a providers: section parses without error and
// leaves Providers nil (zero value), not an error.
func TestProvidersUnmarshal_MissingSection(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("router:\n  enabled: true\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Providers != nil {
		t.Errorf("Providers = %#v, want nil with no providers: section", cfg.Providers)
	}
}

func TestMergeProviders(t *testing.T) {
	base := map[string]ProviderOverride{"anthropic": {BaseURL: "https://old.example.com"}}
	overlay := map[string]ProviderOverride{
		"anthropic": {BaseURL: "https://new.example.com"},
		"openai":    {BaseURL: "https://openai-gateway.example.com"},
	}
	merged := mergeProviders(base, overlay)
	if got := merged["anthropic"].BaseURL; got != "https://new.example.com" {
		t.Errorf("anthropic BaseURL = %q, want overlay to win", got)
	}
	if got := merged["openai"].BaseURL; got != "https://openai-gateway.example.com" {
		t.Errorf("openai BaseURL = %q, want %q", got, "https://openai-gateway.example.com")
	}
}

func TestMergeProviders_EmptyOverlayKeepsBase(t *testing.T) {
	base := map[string]ProviderOverride{"anthropic": {BaseURL: "https://old.example.com"}}
	merged := mergeProviders(base, nil)
	if got := merged["anthropic"].BaseURL; got != "https://old.example.com" {
		t.Errorf("anthropic BaseURL = %q, want base preserved when overlay is empty", got)
	}
}

func TestLoadLearnedAgents(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "search-first.yaml"), []byte("id: search-first\nname: Search-First Agent\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// Malformed (no id) — must be skipped, not fail the whole load.
	if err := os.WriteFile(filepath.Join(dir, "malformed.yaml"), []byte("name: no id here\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// Non-YAML file — must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not yaml"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	agents, err := loadLearnedAgents(dir)
	if err != nil {
		t.Fatalf("loadLearnedAgents() error = %v", err)
	}
	if len(agents) != 1 || agents[0].ID != "search-first" {
		t.Errorf("loadLearnedAgents() = %+v, want one agent with id=search-first", agents)
	}
}

func TestLoadLearnedAgents_MissingDirIsNotError(t *testing.T) {
	agents, err := loadLearnedAgents(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("loadLearnedAgents() error = %v, want nil for a missing directory", err)
	}
	if agents != nil {
		t.Errorf("loadLearnedAgents() = %+v, want nil", agents)
	}
}

func TestMergeLearnedAgents_HandAuthoredWins(t *testing.T) {
	existing := []agent.AgentConfig{{ID: "coder", Name: "Hand-authored coder"}}
	learned := []agent.AgentConfig{
		{ID: "coder", Name: "Learned coder — must not override"},
		{ID: "search-first", Name: "Learned search-first"},
	}

	merged := mergeLearnedAgents(existing, learned)

	if len(merged) != 2 {
		t.Fatalf("mergeLearnedAgents() = %+v, want 2 agents", merged)
	}
	byID := make(map[string]agent.AgentConfig, len(merged))
	for _, a := range merged {
		byID[a.ID] = a
	}
	if byID["coder"].Name != "Hand-authored coder" {
		t.Errorf("coder.Name = %q, want hand-authored config to win", byID["coder"].Name)
	}
	if byID["search-first"].Name != "Learned search-first" {
		t.Errorf("search-first.Name = %q, want the learned agent to be appended", byID["search-first"].Name)
	}
}
