package learning

import "testing"

const sampleLearningYAML = `
distillation:
  model:
    provider: "anthropic"
    model: "claude-sonnet-4-6"
  prompt: |
    You analyze aggregated execution statistics.
`

func TestParse(t *testing.T) {
	cfg, err := Parse([]byte(sampleLearningYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Distillation.Model.Provider != "anthropic" {
		t.Errorf("Model.Provider = %q, want %q", cfg.Distillation.Model.Provider, "anthropic")
	}
	if cfg.Distillation.Model.Model != "claude-sonnet-4-6" {
		t.Errorf("Model.Model = %q, want %q", cfg.Distillation.Model.Model, "claude-sonnet-4-6")
	}
	if cfg.Distillation.Prompt == "" {
		t.Error("Prompt is empty, want the configured prompt text")
	}
}

func TestParse_Invalid(t *testing.T) {
	if _, err := Parse([]byte("not: [valid yaml")); err == nil {
		t.Error("Parse() with malformed YAML: want error, got nil")
	}
}

func TestConfig_ModelConfig(t *testing.T) {
	cfg, err := Parse([]byte(sampleLearningYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	mc := cfg.ModelConfig()
	if mc.Provider != "anthropic" || mc.Model != "claude-sonnet-4-6" {
		t.Errorf("ModelConfig() = %+v, want provider=anthropic model=claude-sonnet-4-6", mc)
	}
}
