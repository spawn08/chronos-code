package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos/sdk/agent"
	"gopkg.in/yaml.v3"
)

func TestEmbeddedDefaultsStartFreshSession(t *testing.T) {
	cfg, err := loadEmbeddedDefaults()
	if err != nil {
		t.Fatalf("loadEmbeddedDefaults() error = %v", err)
	}
	if cfg.Session.AutoResume {
		t.Fatal("embedded session.auto_resume = true, want false for a clean default startup")
	}
	foundPPDPlanner := false
	for _, configured := range cfg.Agents {
		if configured.ID == "ppd-planner" {
			foundPPDPlanner = configured.System != "" && len(configured.Tools) > 0
		}
	}
	if !foundPPDPlanner {
		t.Fatal("embedded defaults do not include a configured ppd-planner")
	}
}

func TestEmbeddedDefaultsStartWithChronosCodePrimary(t *testing.T) {
	cfg, err := loadEmbeddedDefaults()
	if err != nil {
		t.Fatalf("loadEmbeddedDefaults() error = %v", err)
	}
	injectSystemPrompts(cfg)
	if len(cfg.Agents) == 0 || cfg.Agents[0].ID != "chronos-code" {
		t.Fatalf("first embedded agent = %#v, want chronos-code", cfg.Agents)
	}
	if cfg.Agents[0].System == "" {
		t.Fatal("chronos-code system prompt was not injected")
	}
	foundCoder := false
	for _, configured := range cfg.Agents {
		if configured.ID == "coder" {
			foundCoder = true
			break
		}
	}
	if !foundCoder {
		t.Fatal("embedded defaults dropped the coder specialist")
	}
}

func TestEmbeddedDefaultsUseReportVerification(t *testing.T) {
	cfg, err := loadEmbeddedDefaults()
	if err != nil {
		t.Fatalf("loadEmbeddedDefaults() error = %v", err)
	}
	if cfg.Verification.Mode != verification.ModeReport {
		t.Fatalf("verification.mode = %q, want %q", cfg.Verification.Mode, verification.ModeReport)
	}
}

func TestAdaptiveConsumerRollbackControlsDefaultToEnabledAndDecodeFalse(t *testing.T) {
	defaults := Config{}
	if !defaults.Session.RecallPriorSummariesEnabled() || !defaults.Session.ContextReportEnabled() || !defaults.Learning.PatternInjectionEnabled() || !defaults.MCP.DiscoveryEnabled() {
		t.Fatal("adaptive consumers must default to enabled")
	}

	var cfg Config
	if err := yaml.Unmarshal([]byte("session:\n  recall_prior_summaries: false\n  context_report: false\nlearning:\n  pattern_injection: false\nmcp:\n  discovery_enabled: false\n"), &cfg); err != nil {
		t.Fatalf("decode rollback controls: %v", err)
	}
	if cfg.Session.RecallPriorSummariesEnabled() || cfg.Session.ContextReportEnabled() || cfg.Learning.PatternInjectionEnabled() || cfg.MCP.DiscoveryEnabled() {
		t.Fatalf("rollback controls did not disable consumers: %#v", cfg)
	}
}

func TestVerificationModeValidation(t *testing.T) {
	for _, mode := range []verification.Mode{verification.ModeReport, verification.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := mustConfig(t, "verification:\n  mode: "+string(mode)+"\n")
			if cfg.Verification.Mode != mode {
				t.Fatalf("verification.mode = %q, want %q", cfg.Verification.Mode, mode)
			}
		})
	}

	var cfg Config
	err := yaml.Unmarshal([]byte("verification:\n  mode: ignore\n"), &cfg)
	if err == nil || !strings.Contains(err.Error(), "verification.mode") || !strings.Contains(err.Error(), "report") || !strings.Contains(err.Error(), "enforce") {
		t.Fatalf("unmarshal error = %v, want verification.mode report|enforce validation", err)
	}
}

func TestLoadFromDirRejectsInvalidVerificationMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("verification:\n  mode: ignore\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadFromDir(dir)
	if err == nil || !strings.Contains(err.Error(), "verification.mode") {
		t.Fatalf("loadFromDir() error = %v, want invalid verification mode", err)
	}
}

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

func TestMergeConfigOverlaysTypedFieldsIncludingFalseAndZero(t *testing.T) {
	base := mustConfig(t, `
router:
  enabled: true
  budget_tokens: 100
  model:
    provider: anthropic
    model: sonnet
security:
  denied_paths: [vendor]
memory:
  enabled: true
  auto_extract: true
session:
  auto_resume: true
  max_history_turns: 50
workspace:
  index_on_start: true
tools:
  compression_threshold_tokens: 500
learning:
  enabled: true
  min_sessions_before_distill: 3
verification:
  mode: report
server:
  enabled: true
  max_concurrent: 10
`)
	setConfigSource(base, "embedded")
	overlay := mustConfig(t, `
router:
  enabled: false
  budget_tokens: 0
  model:
    model: haiku
security:
  denied_paths: []
memory:
  enabled: false
  auto_extract: false
session:
  auto_resume: false
  max_history_turns: 0
workspace:
  index_on_start: false
tools:
  compression_threshold_tokens: 0
learning:
  enabled: false
  min_sessions_before_distill: 0
verification:
  mode: enforce
server:
  enabled: false
  max_concurrent: 0
`)

	mergeConfig(base, overlay, "project")

	if base.Router.Enabled || base.Router.BudgetTokens != 0 || base.Router.Model.Provider != "anthropic" || base.Router.Model.Model != "haiku" {
		t.Errorf("Router = %+v, want false/zero overlay with inherited provider", base.Router)
	}
	if len(base.Security.DeniedPaths) != 0 || base.Memory.Enabled || base.Memory.AutoExtract || base.Session.AutoResume || base.Session.MaxHistoryTurns != 0 || *base.Workspace.IndexOnStart || base.Tools.CompressionThresholdTokens != 0 || base.Learning.Enabled || base.Learning.MinSessionsBeforeDistill != 0 || base.Server.Enabled || base.Server.MaxConcurrent != 0 {
		t.Errorf("typed overlay did not preserve explicit false, zero, or empty values: %+v", base)
	}
	if base.Verification.Mode != verification.ModeEnforce {
		t.Errorf("Verification.Mode = %q, want %q", base.Verification.Mode, verification.ModeEnforce)
	}
	if got := base.sources["router.enabled"]; got != "project" {
		t.Errorf("source router.enabled = %q, want project", got)
	}
	if got := base.sources["router.model.provider"]; got != "embedded" {
		t.Errorf("source router.model.provider = %q, want embedded", got)
	}
}

func TestEffectiveConfigRedactsCredentialsAndReportsSources(t *testing.T) {
	cfg := mustConfig(t, `
server:
  api_key: top-secret
  listen: 127.0.0.1:8080
`)
	setConfigSource(cfg, "cli")

	effective, err := cfg.EffectiveConfig()
	if err != nil {
		t.Fatalf("EffectiveConfig() error = %v", err)
	}
	server := effective.Values["server"].(map[string]any)
	if got := server["api_key"]; got != "[REDACTED]" {
		t.Errorf("redacted api_key = %#v, want [REDACTED]", got)
	}
	if got := server["listen"]; got != "127.0.0.1:8080" {
		t.Errorf("listen = %#v, want unredacted value", got)
	}
	if got := effective.Sources["server.api_key"]; got != "cli" {
		t.Errorf("source server.api_key = %q, want cli", got)
	}
}

func mustConfig(t *testing.T, body string) *Config {
	t.Helper()
	var cfg Config
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	return &cfg
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

func TestLoadAgentDirSupportsYAMLAndYML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "beta.yml"), []byte("id: beta\nname: Beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alpha.yaml"), []byte("id: alpha\nname: Alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := loadAgentDir(dir)
	if err != nil {
		t.Fatalf("loadAgentDir() error = %v", err)
	}
	if len(got) != 2 || got[0].ID != "alpha" || got[1].ID != "beta" {
		t.Fatalf("loadAgentDir() = %+v, want alpha then beta", got)
	}
}

func TestLoadAgentDirRejectsDefinitionWithoutID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(path, []byte("name: Missing ID\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadAgentDir(dir)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "missing id") {
		t.Fatalf("loadAgentDir() error = %v, want path and missing id", err)
	}
}

func TestMergeAgentsUsesCaseInsensitiveIDPrecedence(t *testing.T) {
	base := []agent.AgentConfig{{ID: "coder", Name: "Built in"}, {ID: "reviewer", Name: "Reviewer"}}
	overlay := []agent.AgentConfig{{ID: "CODER", Name: "User Coder"}, {ID: "custom", Name: "Custom"}}

	got := mergeAgents(base, overlay)

	if len(got) != 3 {
		t.Fatalf("mergeAgents() = %+v, want three unique agents", got)
	}
	if got[0].ID != "coder" || got[0].Name != "User Coder" {
		t.Errorf("merged coder = %+v, want user definition in original position", got[0])
	}
	if got[1].ID != "reviewer" || got[2].ID != "custom" {
		t.Errorf("merged order = %+v, want reviewer then custom", got)
	}
}

func TestLoadDiscoversUserAndProjectAgentDirectories(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(repo)

	userAgents := filepath.Join(home, ConfigDirName, "agents")
	projectAgents := filepath.Join(repo, ConfigDirName, "agents")
	for _, dir := range []string{userAgents, projectAgents} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fixtures := map[string]string{
		filepath.Join(userAgents, "user-worker.yaml"):       "id: user-worker\nname: User Worker\n",
		filepath.Join(userAgents, "shared.yml"):             "id: shared\nname: User Shared\n",
		filepath.Join(projectAgents, "project-worker.yaml"): "id: project-worker\nname: Project Worker\n",
		filepath.Join(projectAgents, "shared.yaml"):         "id: shared\nname: Project Shared\n",
	}
	for path, body := range fixtures {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	byID := make(map[string]agent.AgentConfig, len(cfg.Agents))
	for _, configured := range cfg.Agents {
		byID[strings.ToLower(configured.ID)] = configured
	}
	for _, id := range []string{"coder", "user-worker", "project-worker", "shared"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("resolved agents missing %q: %v", id, byID)
		}
	}
	if got := byID["shared"].Name; got != "Project Shared" {
		t.Errorf("shared agent name = %q, want project override", got)
	}
}
