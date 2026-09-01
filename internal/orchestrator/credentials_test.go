package orchestrator

import (
	"context"
	"testing"

	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/config"
)

// TestApplyStoredCredentials_BaseURLOverride covers AC-2.2: a
// providers.<name>.base_url override in config.yaml must reach the
// ModelConfig that agent.BuildProvider receives.
func TestApplyStoredCredentials_BaseURLOverride(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderOverride{
			"anthropic": {BaseURL: "https://internal-gateway.corp.com/claude"},
		},
	}
	cfg.Defaults = &agent.AgentConfig{
		Model: agent.ModelConfig{Provider: "anthropic", APIKey: "sk-already-set"},
	}

	applyStoredCredentials(context.Background(), cfg)

	if got, want := cfg.Defaults.Model.BaseURL, "https://internal-gateway.corp.com/claude"; got != want {
		t.Errorf("Defaults.Model.BaseURL = %q, want %q", got, want)
	}
	if cfg.Defaults.Model.APIKey != "sk-already-set" {
		t.Errorf("APIKey should be left untouched when already set, got %q", cfg.Defaults.Model.APIKey)
	}
}

// TestApplyStoredCredentials_NoOverride covers the invariant that a
// config.yaml without a providers: section (nil map) must not panic and
// must leave BaseURL empty.
func TestApplyStoredCredentials_NoOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agents = []agent.AgentConfig{
		{ID: "coder", Model: agent.ModelConfig{Provider: "anthropic", APIKey: "sk-already-set"}},
	}

	applyStoredCredentials(context.Background(), cfg)

	if cfg.Agents[0].Model.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty with no providers: section", cfg.Agents[0].Model.BaseURL)
	}
}
