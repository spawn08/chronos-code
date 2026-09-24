package auth

import (
	"sort"
	"strings"
)

// CanonicalProvider normalizes provider names accepted at public auth
// boundaries so aliases share credentials and status.
func CanonicalProvider(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude":
		return "anthropic"
	case "codex":
		return "openai"
	case "azure-openai":
		return "azure"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

func providerStorageNames(provider string) []string {
	switch CanonicalProvider(provider) {
	case "anthropic":
		return []string{"anthropic", "claude"}
	case "openai":
		return []string{"openai", "codex"}
	case "azure":
		return []string{"azure", "azure-openai"}
	default:
		return []string{CanonicalProvider(provider)}
	}
}

var builtinProviders = map[string]ProviderOAuthConfig{
	"anthropic": {
		Provider:     "anthropic",
		ClientID:     "chronos-code",
		AuthURL:      "https://console.anthropic.com/oauth/authorize",
		TokenURL:     "https://console.anthropic.com/oauth/token",
		Scopes:       []string{"user:inference"},
		RedirectPort: 8765,
	},
	"openai": OpenAICodexSubscriptionConfig(),
}

// LookupProvider returns the built-in OAuth configuration for a known
// provider name (case-insensitive). Returns false if the provider has no
// built-in registration and the caller must supply credentials manually.
func LookupProvider(name string) (ProviderOAuthConfig, bool) {
	cfg, ok := builtinProviders[CanonicalProvider(name)]
	return cfg, ok
}

// ListProviders returns the sorted names of all built-in OAuth providers.
func ListProviders() []string {
	names := make([]string, 0, len(builtinProviders))
	for k := range builtinProviders {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
