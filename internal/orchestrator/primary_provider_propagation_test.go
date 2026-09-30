package orchestrator

import (
	"testing"

	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/config"
)

func agentByID(t *testing.T, cfg *config.Config, id string) agent.AgentConfig {
	t.Helper()
	for _, a := range cfg.Agents {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("agent %q not found", id)
	return agent.AgentConfig{}
}

// An Azure primary configured in YAML (no CLI/env override) must still stop
// uncredentialed Anthropic subagents from being built against Anthropic, and
// they must inherit the primary's full Azure config (endpoint, deployment).
func TestApplyPrimaryProvider_AzurePrimaryFromYAML(t *testing.T) {
	primary := agent.ModelConfig{Provider: "azure", Model: "gpt-6.1-sol", Deployment: "gpt-6.1-sol", APIKey: "azure-key", BaseURL: "https://example.openai.azure.com"}
	cfg := &config.Config{}
	cfg.Agents = []agent.AgentConfig{
		{ID: "chronos-code", Model: primary},
		{ID: "researcher", Model: agent.ModelConfig{Provider: "anthropic", Model: "claude-haiku-4-5"}},
		{ID: "explainer", Model: agent.ModelConfig{Provider: "claude", Model: "claude-haiku-4-5"}},
	}

	applyPrimaryProviderToUncredentialedAgents(cfg)

	for _, id := range []string{"researcher", "explainer"} {
		if got := agentByID(t, cfg, id).Model; got != primary {
			t.Errorf("%s model = %+v, want primary %+v", id, got, primary)
		}
	}
	if got := agentByID(t, cfg, "chronos-code").Model; got != primary {
		t.Errorf("primary mutated: %+v", got)
	}
}

// The same propagation applies after a --provider/env override.
func TestApplyPrimaryProvider_AfterOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agents = []agent.AgentConfig{
		{ID: "chronos-code", Model: agent.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6"}},
		{ID: "researcher", Model: agent.ModelConfig{Provider: "anthropic", Model: "claude-haiku-4-5"}},
	}
	if err := cfg.OverridePrimaryModel("openai", "gpt-4o", "flag:--provider", "flag:--model"); err != nil {
		t.Fatalf("OverridePrimaryModel() error = %v", err)
	}
	for i := range cfg.Agents {
		if cfg.Agents[i].ID == "chronos-code" {
			cfg.Agents[i].Model.APIKey = "oai-key" // as applyStoredCredentials would resolve
		}
	}

	applyPrimaryProviderToUncredentialedAgents(cfg)

	if got := agentByID(t, cfg, "researcher").Model; got.Provider != "openai" || got.Model != "gpt-4o" || got.APIKey != "oai-key" {
		t.Errorf("researcher model = %+v, want primary openai/gpt-4o with its key", got)
	}
}

func TestApplyPrimaryProvider_LeavesWorkingAgentsAlone(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agents = []agent.AgentConfig{
		{ID: "chronos-code", Model: agent.ModelConfig{Provider: "azure", Model: "gpt-4o", APIKey: "azure-key"}},
		{ID: "credentialed", Model: agent.ModelConfig{Provider: "openai", Model: "gpt-4o-mini", APIKey: "own-key"}},
		{ID: "local", Model: agent.ModelConfig{Provider: "ollama", Model: "llama3"}},
		{ID: "same-provider", Model: agent.ModelConfig{Provider: "azure", Model: "other-deployment"}},
	}
	before := append([]agent.AgentConfig(nil), cfg.Agents...)

	applyPrimaryProviderToUncredentialedAgents(cfg)

	for i := range before {
		if cfg.Agents[i].Model != before[i].Model {
			t.Errorf("%s mutated: %+v, want %+v", before[i].ID, cfg.Agents[i].Model, before[i].Model)
		}
	}
}

func TestApplyPrimaryProvider_NoopWhenPrimaryUncredentialed(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agents = []agent.AgentConfig{
		{ID: "chronos-code", Model: agent.ModelConfig{Provider: "azure", Model: "gpt-4o"}},
		{ID: "researcher", Model: agent.ModelConfig{Provider: "anthropic", Model: "claude-haiku-4-5"}},
	}

	applyPrimaryProviderToUncredentialedAgents(cfg)

	if got := agentByID(t, cfg, "researcher").Model.Provider; got != "anthropic" {
		t.Errorf("researcher provider = %q, want anthropic untouched", got)
	}
}
