package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos/sdk/agent"
)

// isolateProviderEnv removes every credential and Azure setting from the
// environment so selection tests do not depend on the developer's machine.
func isolateProviderEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{
		"CHRONOS_CODE_PROVIDER", "CHRONOS_CODE_MODEL",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"OPENAI_API_KEY", "CODEX_ACCESS_TOKEN", "GEMINI_API_KEY", "GOOGLE_API_KEY",
		"MISTRAL_API_KEY", "AZURE_OPENAI_API_KEY", "GROQ_API_KEY", "TOGETHER_API_KEY",
		"DEEPSEEK_API_KEY", "OPENROUTER_API_KEY", "FIREWORKS_API_KEY", "PERPLEXITY_API_KEY",
		"ANYSCALE_API_KEY", "AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_BASE_URL",
		"AZURE_OPENAI_DEPLOYMENT", "AZURE_OPENAI_API_VERSION",
	} {
		t.Setenv(name, "")
	}
}

func selectedPrimary(t *testing.T, args ...string) (provider, model, providerSource string, err error) {
	t.Helper()
	resetGlobalFlags(t, append([]string{"chronos-code"}, args...))
	if err := stripGlobalFlags(); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigWithModelSelection()
	if err != nil {
		return "", "", "", err
	}
	_, selected, providerSource, _ := cfg.PrimaryAgentModel()
	return selected.Provider, selected.Model, providerSource, nil
}

func TestAzureModelOnlyUsesAuthorizedProvider(t *testing.T) {
	for _, tc := range []struct {
		name         string
		env          map[string]string
		args         []string
		wantProvider string
		wantModel    string
	}{
		{
			name:         "unlisted deployment with only an azure key",
			env:          map[string]string{"AZURE_OPENAI_API_KEY": "k"},
			args:         []string{"--model", "my-deployment", "config", "show"},
			wantProvider: "azure",
			wantModel:    "my-deployment",
		},
		{
			name:         "model shared by openai and azure with only an azure key",
			env:          map[string]string{"AZURE_OPENAI_API_KEY": "k"},
			args:         []string{"--model", "gpt-4o", "config", "show"},
			wantProvider: "azure",
			wantModel:    "gpt-4o",
		},
		{
			name:         "deployment from the environment is azure-only",
			env:          map[string]string{"AZURE_OPENAI_API_KEY": "k", "AZURE_OPENAI_DEPLOYMENT": "corp-gpt"},
			args:         []string{"--model", "corp-gpt", "config", "show"},
			wantProvider: "azure",
			wantModel:    "corp-gpt",
		},
		{
			name:         "anthropic credential keeps an unlisted model on anthropic",
			env:          map[string]string{"ANTHROPIC_API_KEY": "a", "AZURE_OPENAI_API_KEY": "k"},
			args:         []string{"--model", "my-deployment", "config", "show"},
			wantProvider: "anthropic",
			wantModel:    "my-deployment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateProviderEnv(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			provider, model, _, err := selectedPrimary(t, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if provider != tc.wantProvider || model != tc.wantModel {
				t.Fatalf("selected %s/%s, want %s/%s", provider, model, tc.wantProvider, tc.wantModel)
			}
		})
	}
}

func TestAzureAutoSelection(t *testing.T) {
	for _, tc := range []struct {
		name         string
		env          map[string]string
		wantProvider string
		wantSource   string
	}{
		{
			name:         "azure key and deployment",
			env:          map[string]string{"AZURE_OPENAI_API_KEY": "k", "AZURE_OPENAI_DEPLOYMENT": "dep"},
			wantProvider: "azure",
			wantSource:   "auto:azure environment",
		},
		{
			name:         "azure environment wins over another ambient key",
			env:          map[string]string{"AZURE_OPENAI_API_KEY": "k", "OPENAI_API_KEY": "o", "AZURE_OPENAI_DEPLOYMENT": "dep"},
			wantProvider: "azure",
			wantSource:   "auto:azure environment",
		},
		{
			name:         "deployment without any key still names azure",
			env:          map[string]string{"AZURE_OPENAI_ENDPOINT": "https://x.openai.azure.com", "AZURE_OPENAI_DEPLOYMENT": "dep"},
			wantProvider: "azure",
			wantSource:   "auto:azure environment",
		},
		{
			name:         "two keys without azure environment stay on the configured provider",
			env:          map[string]string{"AZURE_OPENAI_API_KEY": "k", "OPENAI_API_KEY": "o"},
			wantProvider: "anthropic",
		},
		{
			name:         "ambient endpoint alone does not take over",
			env:          map[string]string{"AZURE_OPENAI_ENDPOINT": "https://x.openai.azure.com"},
			wantProvider: "anthropic",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateProviderEnv(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			provider, _, source, err := selectedPrimary(t, "config", "show")
			if err != nil {
				t.Fatal(err)
			}
			if provider != tc.wantProvider {
				t.Fatalf("provider = %q, want %q", provider, tc.wantProvider)
			}
			if tc.wantSource != "" && source != tc.wantSource {
				t.Fatalf("provider source = %q, want %q", source, tc.wantSource)
			}
		})
	}
}

func TestAzureKeyWithoutDeploymentExplainsWhatToSet(t *testing.T) {
	isolateProviderEnv(t)
	t.Setenv("AZURE_OPENAI_API_KEY", "k")
	t.Setenv("AZURE_OPENAI_ENDPOINT", "https://x.openai.azure.com")
	resetGlobalFlags(t, []string{"chronos-code"})
	if err := stripGlobalFlags(); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfigWithModelSelection()
	if err == nil || !errors.Is(err, errProviderModelRequired) {
		t.Fatalf("error = %v, want errProviderModelRequired", err)
	}
	for _, want := range []string{"AZURE_OPENAI_DEPLOYMENT", "--model", "CHRONOS_CODE_MODEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "no Azure API key") {
		t.Fatalf("error %q claims the key is missing although AZURE_OPENAI_API_KEY is set", err)
	}

	// Inspection commands must still work so the user can find a deployment.
	cfg, err := loadConfigForInspection()
	if err != nil || cfg == nil {
		t.Fatalf("loadConfigForInspection() = %v, %v", cfg, err)
	}
}

func TestCredentialProviderAzureEnvironment(t *testing.T) {
	cfg := &config.Config{}
	anthropic := agent.ModelConfig{Provider: "anthropic", Model: "claude"}
	none := func(string) bool { return false }

	isolateProviderEnv(t)
	if got := credentialProvider(cfg, anthropic, none); got != "" {
		t.Fatalf("no environment: credentialProvider() = %q, want empty", got)
	}
	t.Setenv("AZURE_OPENAI_DEPLOYMENT", "dep")
	if got := credentialProvider(cfg, anthropic, none); got != "azure" {
		t.Fatalf("deployment only: credentialProvider() = %q, want azure", got)
	}
	both := func(p string) bool { return p == "azure" || p == "openai" }
	if got := credentialProvider(cfg, anthropic, both); got != "azure" {
		t.Fatalf("azure+openai keys with azure environment: credentialProvider() = %q, want azure", got)
	}
	if got := credentialProvider(cfg, anthropic, func(p string) bool { return p == "anthropic" }); got != "" {
		t.Fatalf("anthropic authorized: credentialProvider() = %q, want empty", got)
	}
}
