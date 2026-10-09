package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/router"
)

func TestRoutedProviderReusesProviderUntilConfigChanges(t *testing.T) {
	orch := newRoutingTestOrchestrator(t, map[router.Complexity]map[router.TaskKind]router.ModelSpec{
		router.ComplexityHigh: {router.TaskKindDebug: {Provider: "routed", Model: "high-debug"}},
	})
	builds := 0
	orch.buildProvider = func(mc agent.ModelConfig) (model.Provider, error) {
		builds++
		return &routingTestProvider{provider: mc.Provider, model: mc.Model}, nil
	}
	classification := router.ClassifyTask("fix this bug across multiple files")
	first := orch.routedProvider(context.Background(), "debugger", classification)
	second := orch.routedProvider(context.Background(), "debugger", classification)
	if first == nil || first != second || builds != 1 {
		t.Fatalf("routed providers = (%p, %p) after %d builds, want one reused provider", first, second, builds)
	}

	orch.cfg = &config.Config{Providers: map[string]config.ProviderOverride{"routed": {BaseURL: "https://gateway.example/v1"}}}
	changed, err := orch.cachedRoutedProvider(context.Background(), "debugger", "routed", "high-debug")
	if err != nil {
		t.Fatal(err)
	}
	if changed == first || builds != 2 {
		t.Fatalf("changed config reused stale provider (builds = %d)", builds)
	}

	orch.clearRoutedProviders()
	if _, err := orch.cachedRoutedProvider(context.Background(), "debugger", "routed", "high-debug"); err != nil {
		t.Fatal(err)
	}
	if builds != 3 {
		t.Fatalf("builds after clear = %d, want 3", builds)
	}
}

type mapKeyring map[string]string

func (k mapKeyring) Set(service, user, pass string) error {
	k[service+"/"+user] = pass
	return nil
}

func (k mapKeyring) Get(service, user string) (string, error) {
	v, ok := k[service+"/"+user]
	if !ok {
		return "", auth.ErrNotFound
	}
	return v, nil
}

func (k mapKeyring) Delete(service, user string) error {
	delete(k, service+"/"+user)
	return nil
}

func TestResolveModelConfigUsesStoredAzureSettings(t *testing.T) {
	for _, name := range []string{"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_BASE_URL", "AZURE_OPENAI_API_VERSION"} {
		t.Setenv(name, "")
	}
	store := auth.NewStoreWithBackend(mapKeyring{}, filepath.Join(t.TempDir(), "providers.json"))
	if err := auth.LoginAzure(store, "az-key", auth.AzureSettings{Endpoint: "https://stored.openai.azure.com", Deployment: "gpt-4o", APIVersion: "2024-10-21"}); err != nil {
		t.Fatal(err)
	}

	mc := resolveModelConfig(context.Background(), nil, store, "", "azure", "gpt-4o")
	if mc.APIKey != "az-key" || mc.Endpoint != "https://stored.openai.azure.com" || mc.APIVersion != "2024-10-21" || mc.Deployment != "gpt-4o" {
		t.Fatalf("resolved azure config = %+v", mc)
	}

	cfg := &config.Config{FileConfig: agent.FileConfig{Agents: []agent.AgentConfig{{
		ID: "coder", Model: agent.ModelConfig{Provider: "azure", Endpoint: "https://config.openai.azure.com", APIVersion: "preview"},
	}}}}
	mc = resolveModelConfig(context.Background(), cfg, store, "coder", "azure", "gpt-4o")
	if mc.Endpoint != "https://config.openai.azure.com" || mc.APIVersion != "preview" {
		t.Fatalf("stored settings overrode config: %+v", mc)
	}

	t.Setenv("AZURE_OPENAI_ENDPOINT", "https://env.openai.azure.com")
	t.Setenv("AZURE_OPENAI_API_VERSION", "2025-01-01")
	mc = resolveModelConfig(context.Background(), nil, store, "", "azure", "gpt-4o")
	if mc.Endpoint != "" || mc.APIVersion != "" {
		t.Fatalf("stored settings overrode environment: %+v", mc)
	}
}
