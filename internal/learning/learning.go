// Package learning implements chronos-code's self-learning loop (PRD G-009,
// P3-001/002/003): aggregating execution-trace statistics into a Report
// (Analyze), turning a Report into a candidate agent or pattern Suggestion
// via an LLM (Distiller), and persisting suggestions for human review
// (Store) before anything is ever wired into the active agent set.
//
// This is deliberately not automatic or background: chronos-code only calls
// into this package from the explicit `chronos-code learn` CLI commands.
package learning

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos/sdk/agent"
)

// modelConfig mirrors the `distillation.model` key in learning.yaml.
type modelConfig struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// distillationSection mirrors the `distillation:` top-level key in
// learning.yaml.
type distillationSection struct {
	Model  modelConfig `yaml:"model"`
	Prompt string      `yaml:"prompt"`
}

// Config is the subset of learning.yaml this package understands.
type Config struct {
	Distillation distillationSection `yaml:"distillation"`
}

// Parse decodes raw YAML bytes (e.g. loaded via internal/defaults.ReadFile)
// into a Config.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("learning: parse distillation config: %w", err)
	}
	return &cfg, nil
}

// ModelConfig returns the distillation model as an agent.ModelConfig, ready
// to pass to agent.BuildProvider.
func (c *Config) ModelConfig() agent.ModelConfig {
	return agent.ModelConfig{Provider: c.Distillation.Model.Provider, Model: c.Distillation.Model.Model}
}
