package auth

import (
	"sort"
	"strings"
)

var builtinProviders = map[string]ProviderOAuthConfig{
	"anthropic": {
		Provider:     "anthropic",
		ClientID:     "chronos-code",
		AuthURL:      "https://console.anthropic.com/oauth/authorize",
		TokenURL:     "https://console.anthropic.com/oauth/token",
		Scopes:       []string{"user:inference"},
		RedirectPort: 8765,
	},
}

// LookupProvider returns the built-in OAuth configuration for a known
// provider name (case-insensitive). Returns false if the provider has no
// built-in registration and the caller must supply credentials manually.
func LookupProvider(name string) (ProviderOAuthConfig, bool) {
	cfg, ok := builtinProviders[strings.ToLower(name)]
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
