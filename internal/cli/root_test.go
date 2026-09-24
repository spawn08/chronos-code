package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos/engine/mcp"
	"github.com/spawn08/chronos/sdk/agent"
)

func TestStripGlobalFlagsJSON(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code", "--json", "run", "hello"})
	if err := stripGlobalFlags(); err != nil {
		t.Fatal(err)
	}
	if !jsonMode {
		t.Fatal("--json was not parsed")
	}
	if got := strings.Join(os.Args, " "); got != "chronos-code run hello" {
		t.Fatalf("args after strip = %q", got)
	}
}

func TestStripGlobalFlagsModelSelection(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code", "run", "--provider= Claude ", "--model", "claude-test", "task"})
	if err := stripGlobalFlags(); err != nil {
		t.Fatal(err)
	}
	if !providerOverrideSet || providerOverride != "anthropic" || !modelOverrideSet || modelOverride != "claude-test" {
		t.Fatalf("selection = provider(%t,%q) model(%t,%q)", providerOverrideSet, providerOverride, modelOverrideSet, modelOverride)
	}
	if got := strings.Join(os.Args, " "); got != "chronos-code run task" {
		t.Fatalf("args after strip = %q", got)
	}
}

func TestLoadConfigModelSelectionPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`agents:
  - id: chronos-code
    name: Primary
    model: {provider: anthropic, model: yaml-model}
  - id: openai-worker
    name: OpenAI Worker
    model: {provider: openai, model: configured-openai}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHRONOS_CODE_PROVIDER", "codex")
	t.Setenv("CHRONOS_CODE_MODEL", "env-model")
	resetGlobalFlags(t, []string{"chronos-code", "--provider", "claude", "--model", "cli-model", "config", "show"})
	configPath = path
	if err := stripGlobalFlags(); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigWithModelSelection()
	if err != nil {
		t.Fatal(err)
	}
	_, selected, providerSource, modelSource := cfg.PrimaryAgentModel()
	if selected.Provider != "anthropic" || selected.Model != "cli-model" || providerSource != "flag:--provider" || modelSource != "flag:--model" {
		t.Fatalf("selected = %+v, sources=(%q,%q)", selected, providerSource, modelSource)
	}
	if !cfg.PrimaryModelSelected() {
		t.Fatal("explicit selection must bypass bundled model routing")
	}
}

func TestCredentialProviderSelectsOnlyUnambiguousAlternative(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agents = []agent.AgentConfig{
		{ID: "chronos-code", Model: agent.ModelConfig{Provider: "azure", Model: "deployment"}},
		{ID: "worker", Model: agent.ModelConfig{Provider: "openai", Model: "gpt-5"}},
	}
	current := cfg.Agents[0].Model
	for _, tc := range []struct {
		name       string
		authorized map[string]bool
		model      agent.ModelConfig
		want       string
	}{
		{"only anthropic key", map[string]bool{"anthropic": true}, current, "anthropic"},
		{"configured key wins", map[string]bool{"azure": true, "anthropic": true}, current, ""},
		{"ambiguous keys", map[string]bool{"anthropic": true, "openai": true}, current, ""},
		{"yaml key wins", map[string]bool{"anthropic": true}, agent.ModelConfig{Provider: "azure", APIKey: "configured-key"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := credentialProvider(cfg, tc.model, func(provider string) bool { return tc.authorized[provider] })
			if got != tc.want {
				t.Fatalf("credentialProvider() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadConfigProviderOnlyUsesKnownDefault(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code", "--provider", "openai", "config", "show"})
	t.Setenv("CHRONOS_CODE_PROVIDER", "")
	t.Setenv("CHRONOS_CODE_MODEL", "")
	if err := stripGlobalFlags(); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigWithModelSelection()
	if err != nil {
		t.Fatal(err)
	}
	_, selected, _, source := cfg.PrimaryAgentModel()
	if selected.Provider != "openai" || selected.Model != "gpt-4o" || source != "provider default" {
		t.Fatalf("selection = (%s/%s, %s)", selected.Provider, selected.Model, source)
	}
}

func TestLoadConfigSelectsSoleAuthorizedProvider(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code", "config", "show"})
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CHRONOS_CODE_PROVIDER", "")
	t.Setenv("CHRONOS_CODE_MODEL", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("OPENAI_API_KEY", "test-key")
	for _, name := range []string{"CODEX_ACCESS_TOKEN", "GEMINI_API_KEY", "GOOGLE_API_KEY", "MISTRAL_API_KEY", "AZURE_OPENAI_API_KEY", "GROQ_API_KEY", "TOGETHER_API_KEY", "DEEPSEEK_API_KEY", "OPENROUTER_API_KEY", "FIREWORKS_API_KEY", "PERPLEXITY_API_KEY", "ANYSCALE_API_KEY"} {
		t.Setenv(name, "")
	}
	cfg, err := loadConfigWithModelSelection()
	if err != nil {
		t.Fatal(err)
	}
	_, selected, providerSource, modelSource := cfg.PrimaryAgentModel()
	if selected.Provider != "openai" || selected.Model != "gpt-4o" || providerSource != "auto:only authorized provider" || modelSource != "provider default" || !cfg.PrimaryModelSelected() {
		t.Fatalf("selection = (%s/%s, %s, %s, %t)", selected.Provider, selected.Model, providerSource, modelSource, cfg.PrimaryModelSelected())
	}
}

func TestLoadConfigModelOnlyInfersKnownProvider(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code", "--model", "gpt-5-mini", "config", "show"})
	t.Setenv("CHRONOS_CODE_PROVIDER", "")
	t.Setenv("CHRONOS_CODE_MODEL", "")
	if err := stripGlobalFlags(); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigWithModelSelection()
	if err != nil {
		t.Fatal(err)
	}
	_, selected, providerSource, _ := cfg.PrimaryAgentModel()
	if selected.Provider != "openai" || selected.Model != "gpt-5-mini" || providerSource != "model:catalog" {
		t.Fatalf("selection = (%s/%s, %s)", selected.Provider, selected.Model, providerSource)
	}
}

func TestLoadConfigAmbiguousModelRequiresProvider(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code", "--model", "gpt-4o", "config", "show"})
	t.Setenv("CHRONOS_CODE_PROVIDER", "")
	t.Setenv("CHRONOS_CODE_MODEL", "")
	if err := stripGlobalFlags(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigWithModelSelection(); err == nil || !strings.Contains(err.Error(), "supply --provider") {
		t.Fatalf("error = %v, want ambiguous-provider instruction", err)
	}
}

func TestLoadConfigProviderOnlyUsesConfiguredModelOrErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`agents:
  - id: chronos-code
    name: Primary
    model: {provider: anthropic, model: yaml-model}
  - id: openai-worker
    name: OpenAI Worker
    model: {provider: openai, model: configured-openai}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHRONOS_CODE_PROVIDER", "")
	t.Setenv("CHRONOS_CODE_MODEL", "")
	resetGlobalFlags(t, []string{"chronos-code", "--provider", "openai", "config", "show"})
	configPath = path
	if err := stripGlobalFlags(); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigWithModelSelection()
	if err != nil {
		t.Fatal(err)
	}
	_, selected, _, source := cfg.PrimaryAgentModel()
	if selected.Provider != "openai" || selected.Model != "configured-openai" || source != "configured agent openai-worker" {
		t.Fatalf("selected = %+v, source=%q", selected, source)
	}

	resetGlobalFlags(t, []string{"chronos-code", "--provider", "openrouter", "config", "show"})
	configPath = path
	if err := stripGlobalFlags(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigWithModelSelection(); err == nil || !strings.Contains(err.Error(), "supply --model") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunModelsUsesExplicitStaticFallback(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code", "models", "mistral"})
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MISTRAL_API_KEY", "")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = originalStdout
		r.Close()
	})
	if err := runModels(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(output); !strings.Contains(got, "mistral [static (live unavailable:") || !strings.Contains(got, "mistral-large-latest") {
		t.Fatalf("models output = %q", got)
	}
}

func TestConfigShowReportsPrimaryModelAndProvenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`defaults:
  model: {provider: openai, model: default-model}
agents:
  - id: chronos-code
    name: Primary
    model: {provider: anthropic, model: primary-model, api_key: must-not-print}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	resetGlobalFlags(t, []string{"chronos-code", "config", "show"})
	t.Setenv("CHRONOS_CODE_PROVIDER", "")
	t.Setenv("CHRONOS_CODE_MODEL", "")
	configPath = path
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = originalStdout
		r.Close()
	})
	if err := runConfig(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	got := string(output)
	for _, want := range []string{"Primary:   chronos-code", "Provider:  anthropic (source: cli)", "Model:     primary-model (source: cli)"} {
		if !strings.Contains(got, want) {
			t.Errorf("config show output %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "must-not-print") || strings.Contains(got, "default-model") {
		t.Fatalf("config show leaked a secret or reported defaults: %q", got)
	}
}

func TestStripGlobalFlagsBudget(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want budget.Microdollars
	}{
		{name: "value before subcommand", args: []string{"chronos-code", "--budget", "5.25", "run", "task"}, want: 5_250_000},
		{name: "value after subcommand", args: []string{"chronos-code", "run", "--budget", "5.25", "task"}, want: 5_250_000},
		{name: "equals before subcommand", args: []string{"chronos-code", "--budget=0.000001", "run", "task"}, want: 1},
		{name: "equals after subcommand", args: []string{"chronos-code", "run", "--budget=0.000001", "task"}, want: 1},
		{name: "whole dollars", args: []string{"chronos-code", "run", "task", "--budget=12"}, want: 12_000_000},
		{name: "maximum", args: []string{"chronos-code", "--budget=9223372036854.775807", "run", "task"}, want: budget.Microdollars(math.MaxInt64)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetGlobalFlags(t, tt.args)

			if err := stripGlobalFlags(); err != nil {
				t.Fatalf("stripGlobalFlags() error = %v", err)
			}
			if !usdBudgetSet {
				t.Fatal("usdBudgetSet = false, want true")
			}
			if usdBudgetCap != tt.want {
				t.Fatalf("usdBudgetCap = %d, want %d", usdBudgetCap, tt.want)
			}
			wantArgs := []string{"chronos-code", "run", "task"}
			if got := strings.Join(os.Args, "\x00"); got != strings.Join(wantArgs, "\x00") {
				t.Fatalf("os.Args = %q, want %q", os.Args, wantArgs)
			}
		})
	}
}

func TestStripGlobalFlagsRejectsInvalidBudget(t *testing.T) {
	values := []string{
		"",
		"-1",
		"+1",
		"one",
		".5",
		"1.",
		"1.2.3",
		"1.0000001",
		"9223372036854.775808",
		"9223372036855",
	}

	for _, value := range values {
		t.Run(value, func(t *testing.T) {
			resetGlobalFlags(t, []string{"chronos-code", "run", "--budget=" + value, "task"})

			if err := stripGlobalFlags(); err == nil {
				t.Fatalf("stripGlobalFlags() error = nil for budget %q", value)
			}
			if usdBudgetSet {
				t.Fatal("usdBudgetSet = true after invalid budget")
			}
		})
	}

	resetGlobalFlags(t, []string{"chronos-code", "run", "task", "--budget"})
	if err := stripGlobalFlags(); err == nil || !strings.Contains(err.Error(), "requires a value") {
		t.Fatalf("stripGlobalFlags() error = %v, want missing-value error", err)
	}
}

func TestStripGlobalFlagsBudgetAbsentIsUnlimited(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code", "run", "--yolo", "task"})

	if err := stripGlobalFlags(); err != nil {
		t.Fatalf("stripGlobalFlags() error = %v", err)
	}
	if usdBudgetSet || usdBudgetCap != 0 {
		t.Fatalf("budget state = (%t, %d), want absent unlimited", usdBudgetSet, usdBudgetCap)
	}
	if !yoloMode || effectivePermissionMode() != "auto_approve" {
		t.Fatal("--yolo behavior changed while parsing global flags")
	}
}

func TestStripGlobalFlagsYoloPositions(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "before subcommand",
			args: []string{"chronos-code", "--yolo", "run", "task"},
			want: []string{"chronos-code", "run", "task"},
		},
		{
			name: "after subcommand",
			args: []string{"chronos-code", "run", "--yolo", "task"},
			want: []string{"chronos-code", "run", "task"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetGlobalFlags(t, tt.args)

			if err := stripGlobalFlags(); err != nil {
				t.Fatalf("stripGlobalFlags() error = %v", err)
			}
			if !yoloMode {
				t.Fatal("yoloMode = false, want true")
			}
			if got := effectivePermissionMode(); got != "auto_approve" {
				t.Fatalf("effectivePermissionMode() = %q, want auto_approve", got)
			}
			if got := strings.Join(os.Args, "\x00"); got != strings.Join(tt.want, "\x00") {
				t.Fatalf("os.Args = %q, want %q", os.Args, tt.want)
			}
		})
	}
}

func TestStripGlobalFlagsRejectsYoloDenyConflict(t *testing.T) {
	tests := [][]string{
		{"chronos-code", "--yolo", "run", "--permission-mode", "deny", "task"},
		{"chronos-code", "run", "--permission-mode=deny", "--yolo", "task"},
	}

	for _, args := range tests {
		resetGlobalFlags(t, args)

		err := stripGlobalFlags()
		if err == nil || !strings.Contains(err.Error(), "--yolo conflicts with --permission-mode deny") {
			t.Fatalf("stripGlobalFlags() error = %v, want yolo/deny conflict", err)
		}
	}
}

func TestStripGlobalFlagsRetainsPermissionModes(t *testing.T) {
	for _, mode := range []string{"prompt", "auto_approve", "deny"} {
		t.Run(mode, func(t *testing.T) {
			resetGlobalFlags(t, []string{"chronos-code", "run", "--permission-mode=" + mode, "task"})

			if err := stripGlobalFlags(); err != nil {
				t.Fatalf("stripGlobalFlags() error = %v", err)
			}
			if permissionMode != mode {
				t.Fatalf("permissionMode = %q, want %q", permissionMode, mode)
			}
		})
	}
}

func TestPrintUsageDocumentsYoloSafetyBoundary(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code"})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	originalStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = originalStdout
		r.Close()
	})

	if err := printUsage(); err != nil {
		t.Fatalf("printUsage() error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close usage writer: %v", err)
	}
	output, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	usage := string(output)
	for _, want := range []string{"--yolo", "policy-allowed", "never overrides deny or destructive confirm"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage missing %q", want)
		}
	}
}

func TestPrintUsageDocumentsBudget(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code"})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	originalStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = originalStdout
		r.Close()
	})

	if err := printUsage(); err != nil {
		t.Fatalf("printUsage() error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close usage writer: %v", err)
	}
	output, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	usage := string(output)
	for _, want := range []string{"--budget <usd>", "6 decimal places", "omitted means unlimited"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage missing %q", want)
		}
	}
}

func TestRunAgentsListsConfiguredAgents(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(configFile, []byte(`agents:
  - id: custom-worker
    name: Custom Worker
    model:
      provider: openai
      model: test-model
`), 0o644); err != nil {
		t.Fatal(err)
	}
	resetGlobalFlags(t, []string{"chronos-code", "agents", "list"})
	configPath = configFile

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = originalStdout
		r.Close()
	})
	if err := runAgents(); err != nil {
		t.Fatalf("runAgents() error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "custom-worker") || !strings.Contains(string(output), "openai/test-model") {
		t.Errorf("agents list output = %q", output)
	}
}

func TestRunMCPCommandAddListRemoveAndUserScope(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	var output bytes.Buffer
	unusedFactory := func(mcp.ServerConfig) (mcpTestClient, error) {
		return nil, errors.New("unexpected client creation")
	}

	if err := runMCPCommand(context.Background(), []string{"add", "local", "--command", "npx", "--arg=-y", "--arg", "server"}, root, home, &output, unusedFactory); err != nil {
		t.Fatalf("add stdio: %v", err)
	}
	if err := runMCPCommand(context.Background(), []string{"add", "remote", "--url", "https://mcp.example.test/events", "--scope", "user"}, root, home, &output, unusedFactory); err != nil {
		t.Fatalf("add SSE: %v", err)
	}
	output.Reset()
	if err := runMCPCommand(context.Background(), []string{"list"}, root, home, &output, unusedFactory); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"local", "stdio", "npx -y server", "permission=require_approval", "status=configured"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("list output %q missing %q", output.String(), want)
		}
	}
	output.Reset()
	if err := runMCPCommand(context.Background(), []string{"list", "--scope=user"}, root, home, &output, unusedFactory); err != nil {
		t.Fatalf("user list: %v", err)
	}
	if !strings.Contains(output.String(), "remote\tsse\thttps://mcp.example.test/events") {
		t.Fatalf("user list output = %q", output.String())
	}
	if err := runMCPCommand(context.Background(), []string{"remove", "local"}, root, home, &output, unusedFactory); err != nil {
		t.Fatalf("remove: %v", err)
	}
}

func TestRunMCPCommandRejectsHTTPAndExpandedSecretsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	unusedFactory := func(mcp.ServerConfig) (mcpTestClient, error) { return nil, nil }
	tests := [][]string{
		{"add", "http", "--transport", "http", "--url", "https://example.test"},
		{"add", "insecure", "--url", "http://example.test"},
		{"add", "secret", "--command", "cmd", "--arg", "--token=plaintext"},
	}
	for _, args := range tests {
		if err := runMCPCommand(context.Background(), args, root, home, io.Discard, unusedFactory); err == nil {
			t.Fatalf("runMCPCommand(%v) error = nil", args)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("invalid additions created config: %v", err)
	}
}

func TestRunMCPCommandListRedactsCredentialValues(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	config := `{"mcpServers":{"local":{"command":"cmd","args":["--token","expanded-secret","--safe","visible"]},"remote":{"url":"https://example.test/mcp?api_key=expanded-query&region=west"}}}`
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runMCPCommand(context.Background(), []string{"list"}, root, home, &output, func(mcp.ServerConfig) (mcpTestClient, error) { return nil, nil })
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(output.String(), "expanded-secret") || strings.Contains(output.String(), "expanded-query") {
		t.Fatalf("list leaked secret: %q", output.String())
	}
	if !strings.Contains(output.String(), "visible") || !strings.Contains(output.String(), "region=west") {
		t.Fatalf("list over-redacted safe values: %q", output.String())
	}
}

func TestRunMCPCommandTestConnectsListsAndCloses(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"local":{"command":"cmd"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeMCPTestClient{tools: []mcp.ToolInfo{{Name: "one"}, {Name: "two"}}}
	var gotConfig mcp.ServerConfig
	var output bytes.Buffer
	err := runMCPCommand(context.Background(), []string{"test", "local", "--timeout", "1s"}, root, home, &output, func(cfg mcp.ServerConfig) (mcpTestClient, error) {
		gotConfig = cfg
		return fake, nil
	})
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if !fake.connected || !fake.listed || !fake.closed {
		t.Fatalf("lifecycle = connected:%t listed:%t closed:%t", fake.connected, fake.listed, fake.closed)
	}
	if gotConfig.Name != "local" || gotConfig.Transport != mcp.TransportStdio {
		t.Fatalf("client config = %#v", gotConfig)
	}
	if got := output.String(); !strings.Contains(got, "status=ok") || !strings.Contains(got, "tools=2") || strings.Contains(got, "cmd") {
		t.Fatalf("test output = %q", got)
	}
}

func TestRunMCPCommandTestIsDeadlineBoundedAndClosesOnFailure(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"hang":{"command":"cmd","args":["--token","expanded-secret"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeMCPTestClient{connect: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	start := time.Now()
	err := runMCPCommand(context.Background(), []string{"test", "hang", "--timeout=20ms"}, root, home, io.Discard, func(mcp.ServerConfig) (mcpTestClient, error) {
		return fake, nil
	})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("test error = %v, want deadline exceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("test exceeded deadline bound: %s", time.Since(start))
	}
	if !fake.closed {
		t.Fatal("client was not closed after connect failure")
	}
	if strings.Contains(err.Error(), "expanded-secret") {
		t.Fatalf("test error leaked secret: %v", err)
	}
}

func TestPrintUsageDocumentsMCPCommands(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code"})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = originalStdout
		r.Close()
	})
	if err := printUsage(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mcp add", "mcp list", "mcp remove", "mcp test", "Initialize, list tools, and close", "stdio and HTTPS SSE only", "HTTP transport is not supported", "${ENV_VAR} references"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("usage missing %q", want)
		}
	}
}

type fakeMCPTestClient struct {
	connect   func(context.Context) error
	tools     []mcp.ToolInfo
	connected bool
	listed    bool
	closed    bool
}

func (f *fakeMCPTestClient) Connect(ctx context.Context) error {
	f.connected = true
	if f.connect != nil {
		return f.connect(ctx)
	}
	return nil
}

func (f *fakeMCPTestClient) ListTools(context.Context) ([]mcp.ToolInfo, error) {
	f.listed = true
	return f.tools, nil
}

func (f *fakeMCPTestClient) Close() error {
	f.closed = true
	return nil
}

func resetGlobalFlags(t *testing.T, args []string) {
	t.Helper()
	originalArgs := os.Args
	originalConfigPath := configPath
	originalDebugMode := debugMode
	originalStreamMode := streamMode
	originalPermissionMode := permissionMode
	originalYoloMode := yoloMode
	originalUSDBudgetCap := usdBudgetCap
	originalUSDBudgetSet := usdBudgetSet
	originalResumeSessionID := resumeSessionID
	originalJSONMode := jsonMode
	originalProviderOverride := providerOverride
	originalModelOverride := modelOverride
	originalProviderOverrideSet := providerOverrideSet
	originalModelOverrideSet := modelOverrideSet

	os.Args = append([]string(nil), args...)
	configPath = ""
	debugMode = false
	streamMode = true
	permissionMode = ""
	yoloMode = false
	usdBudgetCap = 0
	usdBudgetSet = false
	resumeSessionID = ""
	jsonMode = false
	providerOverride = ""
	modelOverride = ""
	providerOverrideSet = false
	modelOverrideSet = false

	t.Cleanup(func() {
		os.Args = originalArgs
		configPath = originalConfigPath
		debugMode = originalDebugMode
		streamMode = originalStreamMode
		permissionMode = originalPermissionMode
		yoloMode = originalYoloMode
		usdBudgetCap = originalUSDBudgetCap
		usdBudgetSet = originalUSDBudgetSet
		resumeSessionID = originalResumeSessionID
		jsonMode = originalJSONMode
		providerOverride = originalProviderOverride
		modelOverride = originalModelOverride
		providerOverrideSet = originalProviderOverrideSet
		modelOverrideSet = originalModelOverrideSet
	})
}
