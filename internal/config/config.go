package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
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
		}
	}

	if projectDir != "" {
		if overlay, err := loadFromDir(projectDir); err == nil {
			mergeFileConfig(&base.FileConfig, &overlay.FileConfig)
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
