package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

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
