package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos/sdk/agent"
)

type Config struct {
	agent.FileConfig `yaml:",inline"`

	Router       RouterConfig                `yaml:"router,omitempty"`
	Security     SecurityConfig              `yaml:"security,omitempty"`
	Memory       MemoryConfig                `yaml:"memory,omitempty"`
	Session      SessionConfig               `yaml:"session,omitempty"`
	Workspace    WorkspaceConfig             `yaml:"workspace,omitempty"`
	Tools        ToolsConfig                 `yaml:"tools,omitempty"`
	Learning     LearningConfig              `yaml:"learning,omitempty"`
	Verification VerificationConfig          `yaml:"verification,omitempty"`
	Server       ServerConfig                `yaml:"server,omitempty"`
	Hooks        HooksConfig                 `yaml:"hooks,omitempty"`
	Providers    map[string]ProviderOverride `yaml:"providers,omitempty"`

	set     map[string]struct{}
	sources map[string]string
}

// EffectiveConfig is a safe representation of the resolved configuration.
// Sources maps YAML paths to the layer that supplied their current value.
type EffectiveConfig struct {
	Values  map[string]any    `yaml:"values"`
	Sources map[string]string `yaml:"sources"`
}

func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	type raw Config
	var decoded raw
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*c = Config(decoded)
	c.set = yamlPaths(node, "")
	return nil
}

// EffectiveConfig returns the resolved configuration without credential values.
func (c *Config) EffectiveConfig() (EffectiveConfig, error) {
	data, err := yaml.Marshal(c)
	if err != nil {
		return EffectiveConfig{}, fmt.Errorf("marshal effective config: %w", err)
	}
	values := make(map[string]any)
	if err := yaml.Unmarshal(data, &values); err != nil {
		return EffectiveConfig{}, fmt.Errorf("unmarshal effective config: %w", err)
	}
	redactValues(values)
	sources := make(map[string]string, len(c.sources))
	for path, source := range c.sources {
		sources[path] = source
	}
	return EffectiveConfig{Values: values, Sources: sources}, nil
}

func yamlPaths(node *yaml.Node, prefix string) map[string]struct{} {
	paths := make(map[string]struct{})
	var visit func(*yaml.Node, string)
	visit = func(n *yaml.Node, path string) {
		if n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i].Value
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			paths[childPath] = struct{}{}
			visit(n.Content[i+1], childPath)
		}
	}
	visit(node, prefix)
	return paths
}

func redactValues(value any) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if isCredentialKey(key) {
				value[key] = "[REDACTED]"
				continue
			}
			redactValues(child)
		}
	case []any:
		for _, child := range value {
			redactValues(child)
		}
	}
}

func isCredentialKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	return strings.Contains(key, "password") || strings.Contains(key, "secret") ||
		strings.Contains(key, "token") || strings.Contains(key, "credential") ||
		key == "api_key" || key == "apikey"
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

// VerificationConfig controls whether supplied or runtime-derived obligations
// are reported or enforced. It does not collect verification evidence.
type VerificationConfig struct {
	Mode verification.Mode `yaml:"mode,omitempty"`
}

func (c *VerificationConfig) UnmarshalYAML(node *yaml.Node) error {
	type raw VerificationConfig
	var decoded raw
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	if decoded.Mode != "" && decoded.Mode != verification.ModeReport && decoded.Mode != verification.ModeEnforce {
		return fmt.Errorf("verification.mode: must be %q or %q", verification.ModeReport, verification.ModeEnforce)
	}
	*c = VerificationConfig(decoded)
	return nil
}

type RouterConfig struct {
	Enabled      bool              `yaml:"enabled,omitempty"`
	Model        agent.ModelConfig `yaml:"model,omitempty"`
	BudgetTokens int               `yaml:"budget_tokens,omitempty"`
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
	AutoResume           bool `yaml:"auto_resume,omitempty"`
	MaxHistoryTurns      int  `yaml:"max_history_turns,omitempty"`
	MaxModelCallsPerTurn int  `yaml:"max_model_calls_per_turn,omitempty"`
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
			mergeConfig(base, overlay, "user")
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load user config: %w", err)
		}
		userAgents, err := loadAgentDir(filepath.Join(userDir, "agents"))
		if err != nil {
			return nil, fmt.Errorf("load user agents: %w", err)
		}
		base.Agents = mergeAgents(base.Agents, userAgents)
	}

	if projectDir != "" {
		if overlay, err := loadFromDir(projectDir); err == nil {
			mergeConfig(base, overlay, "project")
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load project config: %w", err)
		}
		projectAgents, err := loadAgentDir(filepath.Join(projectDir, "agents"))
		if err != nil {
			return nil, fmt.Errorf("load project agents: %w", err)
		}
		base.Agents = mergeAgents(base.Agents, projectAgents)
		if learned, err := loadLearnedAgents(filepath.Join(projectDir, "learned", "agents")); err == nil {
			base.Agents = mergeLearnedAgents(base.Agents, learned)
		}
	}

	if configPath != "" {
		overlay, err := loadFromFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("load config %s: %w", configPath, err)
		}
		mergeConfig(base, overlay, "cli")
	}

	injectSystemPrompts(base)
	agent.NormalizeFileConfig(&base.FileConfig)

	return base, nil
}

// loadAgentDir reads immediate YAML children as complete agent definitions.
// Directory entries are sorted to make newly added agent ordering stable.
func loadAgentDir(dir string) ([]agent.AgentConfig, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out []agent.AgentConfig
	for _, entry := range entries {
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if entry.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read agent %s: %w", path, err)
		}
		var cfg agent.AgentConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse agent %s: %w", path, err)
		}
		if strings.TrimSpace(cfg.ID) == "" {
			return nil, fmt.Errorf("parse agent %s: missing id", path)
		}
		out = append(out, cfg)
	}
	return out, nil
}

// mergeAgents overlays complete agent definitions by case-insensitive ID.
// Replacements retain their original position; new agents append in source
// order so CLI and picker output remains deterministic.
func mergeAgents(base, overlay []agent.AgentConfig) []agent.AgentConfig {
	index := make(map[string]int, len(base))
	for i := range base {
		index[strings.ToLower(base[i].ID)] = i
	}
	for _, cfg := range overlay {
		key := strings.ToLower(cfg.ID)
		if i, ok := index[key]; ok {
			cfg.ID = base[i].ID
			base[i] = cfg
			continue
		}
		index[key] = len(base)
		base = append(base, cfg)
	}
	return base
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
	data, err = defaults.ReadFile("agents/ppd-planner.yaml")
	if err != nil {
		return nil, fmt.Errorf("read embedded ppd-planner agent: %w", err)
	}
	var ppdPlanner agent.AgentConfig
	if err := yaml.Unmarshal(data, &ppdPlanner); err != nil {
		return nil, fmt.Errorf("parse embedded ppd-planner agent: %w", err)
	}
	cfg.Agents = mergeAgents(cfg.Agents, []agent.AgentConfig{ppdPlanner})
	setConfigSource(&cfg, "embedded")
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
		cfg, err := loadFromFile(path)
		if err == nil {
			return cfg, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no config found in %s: %w", dir, os.ErrNotExist)
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
		seen[strings.ToLower(a.ID)] = true
	}
	for _, a := range learned {
		key := strings.ToLower(a.ID)
		if !seen[key] {
			existing = append(existing, a)
			seen[key] = true
		}
	}
	return existing
}

func mergeFileConfig(base, overlay *agent.FileConfig) {
	if len(overlay.Agents) > 0 {
		base.Agents = mergeAgents(base.Agents, overlay.Agents)
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

func mergeConfig(base, overlay *Config, source string) {
	mergeFileConfig(&base.FileConfig, &overlay.FileConfig)
	mergeHooks(&base.Hooks, overlay.Hooks)
	base.Providers = mergeProviders(base.Providers, overlay.Providers)
	mergeTypedSection(&base.Router, overlay.Router, overlay.set, "router")
	mergeTypedSection(&base.Security, overlay.Security, overlay.set, "security")
	mergeTypedSection(&base.Memory, overlay.Memory, overlay.set, "memory")
	mergeTypedSection(&base.Session, overlay.Session, overlay.set, "session")
	mergeTypedSection(&base.Workspace, overlay.Workspace, overlay.set, "workspace")
	mergeTypedSection(&base.Tools, overlay.Tools, overlay.set, "tools")
	mergeTypedSection(&base.Learning, overlay.Learning, overlay.set, "learning")
	mergeTypedSection(&base.Verification, overlay.Verification, overlay.set, "verification")
	mergeTypedSection(&base.Server, overlay.Server, overlay.set, "server")
	if base.sources == nil {
		base.sources = make(map[string]string)
	}
	setConfigPaths(base.sources, overlay.set, source)
}

func setConfigSource(cfg *Config, source string) {
	cfg.sources = make(map[string]string, len(cfg.set))
	setConfigPaths(cfg.sources, cfg.set, source)
}

func setConfigPaths(sources map[string]string, set map[string]struct{}, source string) {
	for path := range set {
		isContainer := false
		for child := range set {
			if strings.HasPrefix(child, path+".") {
				isContainer = true
				break
			}
		}
		if !isContainer {
			sources[path] = source
		}
	}
}

// mergeTypedSection copies only YAML fields declared by the overlay. This
// keeps false, zero, empty strings, and empty collections distinct from omit.
func mergeTypedSection(base, overlay any, set map[string]struct{}, path string) {
	mergeTypedValue(reflect.ValueOf(base).Elem(), reflect.ValueOf(overlay), set, path)
}

func mergeTypedValue(base, overlay reflect.Value, set map[string]struct{}, path string) {
	for i := 0; i < base.NumField(); i++ {
		field := base.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		fieldPath := path + "." + name
		baseField := base.Field(i)
		overlayField := overlay.Field(i)
		if baseField.Kind() == reflect.Struct {
			mergeTypedValue(baseField, overlayField, set, fieldPath)
			continue
		}
		if _, ok := set[fieldPath]; ok {
			baseField.Set(overlayField)
		}
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
