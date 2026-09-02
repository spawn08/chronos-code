package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos/sdk/agent"
)

type Config struct {
	agent.FileConfig `yaml:",inline"`

	Router    RouterConfig    `yaml:"router,omitempty"`
	Security  SecurityConfig  `yaml:"security,omitempty"`
	Memory    MemoryConfig    `yaml:"memory,omitempty"`
	Session   SessionConfig   `yaml:"session,omitempty"`
	Workspace WorkspaceConfig `yaml:"workspace,omitempty"`
	Tools     ToolsConfig     `yaml:"tools,omitempty"`
	Learning  LearningConfig  `yaml:"learning,omitempty"`
	Server    ServerConfig    `yaml:"server,omitempty"`
	Hooks     HooksConfig     `yaml:"hooks,omitempty"`
	Providers map[string]ProviderOverride `yaml:"providers,omitempty"`
}

const maxHookTimeoutMs = 300_000

var hookNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type HookDef struct {
	Name      string `yaml:"name"`
	Command   string `yaml:"command"`
	TimeoutMs int    `yaml:"timeout_ms"`
}

type HooksConfig struct {
	PreToolCall      []HookDef `yaml:"pre_tool_call,omitempty"`
	PostToolCall     []HookDef `yaml:"post_tool_call,omitempty"`
	UserPromptSubmit []HookDef `yaml:"user_prompt_submit,omitempty"`

	preToolCallSet      bool
	postToolCallSet     bool
	userPromptSubmitSet bool
}

func (c *HooksConfig) UnmarshalYAML(node *yaml.Node) error {
	type hooksConfig HooksConfig
	var decoded hooksConfig
	if err := node.Decode(&decoded); err != nil {
		return err
	}

	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			switch node.Content[i].Value {
			case "pre_tool_call":
				decoded.preToolCallSet = true
			case "post_tool_call":
				decoded.postToolCallSet = true
			case "user_prompt_submit":
				decoded.userPromptSubmitSet = true
			default:
				return fmt.Errorf("hooks.%s: unsupported hook point", node.Content[i].Value)
			}
		}
	}

	*c = HooksConfig(decoded)
	return c.validate()
}

func (c HooksConfig) validate() error {
	points := []struct {
		name  string
		hooks []HookDef
	}{
		{name: "pre_tool_call", hooks: c.PreToolCall},
		{name: "post_tool_call", hooks: c.PostToolCall},
		{name: "user_prompt_submit", hooks: c.UserPromptSubmit},
	}

	for _, point := range points {
		seen := make(map[string]struct{}, len(point.hooks))
		for i, hook := range point.hooks {
			field := fmt.Sprintf("hooks.%s[%d]", point.name, i)
			if !hookNamePattern.MatchString(hook.Name) {
				return fmt.Errorf("%s.name: must start with an alphanumeric character and contain only alphanumeric characters, '.', '_', or '-'", field)
			}
			if _, ok := seen[hook.Name]; ok {
				return fmt.Errorf("%s.name: duplicate name %q", field, hook.Name)
			}
			seen[hook.Name] = struct{}{}
			if strings.TrimSpace(hook.Command) == "" {
				return fmt.Errorf("%s.command: must not be empty", field)
			}
			if hook.TimeoutMs <= 0 || hook.TimeoutMs > maxHookTimeoutMs {
				return fmt.Errorf("%s.timeout_ms: must be between 1 and %d", field, maxHookTimeoutMs)
			}
		}
	}
	return nil
}

// ProviderOverride holds per-provider settings that apply regardless of
// which agent or model is in use, such as routing model calls to an
// enterprise gateway/proxy instead of the provider's default endpoint.
type ProviderOverride struct {
	BaseURL string `yaml:"base_url,omitempty"`
}

// ServerConfig controls the HTTP server mode (PRD P3-004).
type ServerConfig struct {
	Enabled         bool   `yaml:"enabled,omitempty"`
	Listen          string `yaml:"listen,omitempty"`
	AuthType        string `yaml:"auth_type,omitempty"`
	APIKey          string `yaml:"api_key,omitempty"`
	CORSOrigins     string `yaml:"cors_origins,omitempty"`
	MaxConcurrent   int    `yaml:"max_concurrent,omitempty"`
	RateLimitPerMin int    `yaml:"rate_limit_per_min,omitempty"`
}

// WorkspaceConfig controls project-root detection and the code graph indexer.
type WorkspaceConfig struct {
	Root         string `yaml:"root,omitempty"`
	IndexOnStart *bool  `yaml:"index_on_start,omitempty"`
	GraphDB      string `yaml:"graph_db,omitempty"`
}

// ToolsConfig controls cross-cutting tool behavior.
type ToolsConfig struct {
	// CompressionThresholdTokens is the token size above which a tool result
	// is evicted to storage and replaced with a preview (PRD P1-006).
	CompressionThresholdTokens int `yaml:"compression_threshold_tokens,omitempty"`
}

type RouterConfig struct {
	Enabled     bool              `yaml:"enabled,omitempty"`
	Model       agent.ModelConfig `yaml:"model,omitempty"`
	BudgetTokens int             `yaml:"budget_tokens,omitempty"`
}

type SecurityConfig struct {
	DeniedPaths    []string `yaml:"denied_paths,omitempty"`
	ShellAllowList []string `yaml:"shell_allow_list,omitempty"`
	MaxExecTimeSec int      `yaml:"max_exec_time_sec,omitempty"`
}

type MemoryConfig struct {
	Enabled     bool   `yaml:"enabled,omitempty"`
	Backend     string `yaml:"backend,omitempty"`
	AutoExtract bool   `yaml:"auto_extract,omitempty"`
}

type SessionConfig struct {
	AutoResume      bool `yaml:"auto_resume,omitempty"`
	MaxHistoryTurns int  `yaml:"max_history_turns,omitempty"`
}

// LearningConfig controls the self-learning distillation loop (PRD G-009,
// P3-001/002/003). The distillation model and prompt themselves live in
// learning.yaml (mirroring how router.yaml's model/prompt sit alongside this
// config's leaner RouterConfig), loaded separately by internal/learning.
type LearningConfig struct {
	Enabled bool `yaml:"enabled,omitempty"`
	// AutoDistill is reserved for a future automatic-trigger mode. Today,
	// chronos-code only ever runs distillation from the explicit
	// `chronos-code learn suggest` command — never silently in the
	// background — so this flag currently has no effect. It's kept so
	// learning.yaml can already declare the intended default without a
	// breaking schema change later.
	AutoDistill bool `yaml:"auto_distill,omitempty"`
	// MinSessionsBeforeDistill is the minimum number of traced sessions for
	// an agent before `learn suggest` will call the distillation model.
	MinSessionsBeforeDistill int `yaml:"min_sessions_before_distill,omitempty"`
	// ReviewBeforeApply is always honored: suggestions are never wired into
	// the active agent set until `chronos-code learn accept` runs. Kept as a
	// config field for schema parity with the PRD; disabling it has no
	// effect.
	ReviewBeforeApply bool   `yaml:"review_before_apply,omitempty"`
	OutputDir         string `yaml:"output_dir,omitempty"`
}

func Load(configPath string) (*Config, error) {
	base, err := loadEmbeddedDefaults()
	if err != nil {
		return nil, fmt.Errorf("load embedded defaults: %w", err)
	}

	projectDir, userDir, err := Discover()
	if err != nil {
		return nil, fmt.Errorf("discover config: %w", err)
	}

	if userDir != "" {
		if overlay, err := loadFromDir(userDir); err == nil {
			mergeFileConfig(&base.FileConfig, &overlay.FileConfig)
			mergeHooks(&base.Hooks, overlay.Hooks)
			base.Providers = mergeProviders(base.Providers, overlay.Providers)
		}
	}

	if projectDir != "" {
		if overlay, err := loadFromDir(projectDir); err == nil {
			mergeFileConfig(&base.FileConfig, &overlay.FileConfig)
			mergeHooks(&base.Hooks, overlay.Hooks)
			base.Providers = mergeProviders(base.Providers, overlay.Providers)
		}
		if learned, err := loadLearnedAgents(filepath.Join(projectDir, "learned", "agents")); err == nil {
			base.Agents = mergeLearnedAgents(base.Agents, learned)
		}
	}

	if configPath != "" {
		overlay, err := loadFromFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("load config %s: %w", configPath, err)
		}
		mergeFileConfig(&base.FileConfig, &overlay.FileConfig)
		mergeHooks(&base.Hooks, overlay.Hooks)
		base.Providers = mergeProviders(base.Providers, overlay.Providers)
	}

	injectSystemPrompts(base)
	agent.NormalizeFileConfig(&base.FileConfig)

	return base, nil
}

func loadEmbeddedDefaults() (*Config, error) {
	data, err := defaults.ReadFile("config.yaml")
	if err != nil {
		return nil, fmt.Errorf("read embedded config.yaml: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse embedded config: %w", err)
	}
	return &cfg, nil
}

func loadFromDir(dir string) (*Config, error) {
	candidates := []string{
		filepath.Join(dir, "config.yaml"),
		filepath.Join(dir, "config.yml"),
		filepath.Join(dir, "agents.yaml"),
		filepath.Join(dir, "agents.yml"),
	}
	for _, path := range candidates {
		if cfg, err := loadFromFile(path); err == nil {
			return cfg, nil
		}
	}
	return nil, fmt.Errorf("no config found in %s", dir)
}

func loadFromFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

func injectSystemPrompts(cfg *Config) {
	for i := range cfg.Agents {
		if cfg.Agents[i].System != "" {
			continue
		}
		agentFile := fmt.Sprintf("agents/%s.yaml", cfg.Agents[i].ID)
		data, err := defaults.ReadFile(agentFile)
		if err != nil {
			continue
		}
		var agentDef struct {
			SystemPrompt string   `yaml:"system_prompt"`
			Instructions []string `yaml:"instructions"`
		}
		if err := yaml.Unmarshal(data, &agentDef); err != nil {
			continue
		}
		cfg.Agents[i].System = agentDef.SystemPrompt
		if len(cfg.Agents[i].Instructions) == 0 && len(agentDef.Instructions) > 0 {
			cfg.Agents[i].Instructions = agentDef.Instructions
		}
	}
}

// loadLearnedAgents reads every *.yaml file under dir — each one a single
// agent.AgentConfig document written by `chronos-code learn accept` (PRD
// P3-002/003) — and returns them as a slice. A missing directory (no
// suggestions accepted yet) is not an error. Files that fail to parse, or
// parse without an id, are skipped rather than failing config load
// entirely: a malformed learned agent shouldn't block the whole harness from
// starting.
func loadLearnedAgents(dir string) ([]agent.AgentConfig, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []agent.AgentConfig
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var ac agent.AgentConfig
		if err := yaml.Unmarshal(data, &ac); err != nil || ac.ID == "" {
			continue
		}
		out = append(out, ac)
	}
	return out, nil
}

// mergeLearnedAgents appends learned agents whose ID isn't already present
// in existing, so a hand-authored agent of the same ID always wins over an
// accepted self-learning suggestion.
func mergeLearnedAgents(existing, learned []agent.AgentConfig) []agent.AgentConfig {
	seen := make(map[string]bool, len(existing))
	for _, a := range existing {
		seen[a.ID] = true
	}
	for _, a := range learned {
		if !seen[a.ID] {
			existing = append(existing, a)
			seen[a.ID] = true
		}
	}
	return existing
}

func mergeFileConfig(base, overlay *agent.FileConfig) {
	if len(overlay.Agents) > 0 {
		base.Agents = overlay.Agents
	}
	if len(overlay.Teams) > 0 {
		base.Teams = overlay.Teams
	}
	if overlay.Defaults != nil {
		base.Defaults = overlay.Defaults
	}
	if overlay.SkillsDir != "" {
		base.SkillsDir = overlay.SkillsDir
	}
}

func mergeHooks(base *HooksConfig, overlay HooksConfig) {
	if overlay.preToolCallSet {
		base.PreToolCall = overlay.PreToolCall
		base.preToolCallSet = true
	}
	if overlay.postToolCallSet {
		base.PostToolCall = overlay.PostToolCall
		base.postToolCallSet = true
	}
	if overlay.userPromptSubmitSet {
		base.UserPromptSubmit = overlay.UserPromptSubmit
		base.userPromptSubmitSet = true
	}
}

// mergeProviders overlays provider-level overrides (currently just
// base_url) onto base, entry by entry, so a project config.yaml can
// override a single provider's base_url without needing to repeat every
// other provider the user config or embedded defaults already set.
func mergeProviders(base, overlay map[string]ProviderOverride) map[string]ProviderOverride {
	if len(overlay) == 0 {
		return base
	}
	if base == nil {
		base = make(map[string]ProviderOverride, len(overlay))
	}
	for name, ov := range overlay {
		if ov.BaseURL != "" {
			existing := base[name]
			existing.BaseURL = ov.BaseURL
			base[name] = existing
		}
	}
	return base
}
