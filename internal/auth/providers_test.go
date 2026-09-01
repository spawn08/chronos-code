package auth

import "testing"

func TestLookupProvider_Anthropic(t *testing.T) {
	cfg, ok := LookupProvider("anthropic")
	if !ok {
		t.Fatal("LookupProvider(\"anthropic\") returned false")
	}
	if cfg.Provider != "anthropic" {
		t.Errorf("Provider = %q, want %q", cfg.Provider, "anthropic")
	}
	if cfg.ClientID == "" {
		t.Error("ClientID is empty")
	}
	if cfg.AuthURL != "https://console.anthropic.com/oauth/authorize" {
		t.Errorf("AuthURL = %q, want Anthropic Console authorize endpoint", cfg.AuthURL)
	}
	if cfg.TokenURL != "https://console.anthropic.com/oauth/token" {
		t.Errorf("TokenURL = %q, want Anthropic Console token endpoint", cfg.TokenURL)
	}
	if len(cfg.Scopes) == 0 {
		t.Error("Scopes is empty")
	}
	if cfg.RedirectPort == 0 {
		t.Error("RedirectPort is zero")
	}
}

func TestLookupProvider_CaseInsensitive(t *testing.T) {
	for _, name := range []string{"ANTHROPIC", "Anthropic", "AnThRoPiC"} {
		if _, ok := LookupProvider(name); !ok {
			t.Errorf("LookupProvider(%q) returned false", name)
		}
	}
}

func TestLookupProvider_Unknown(t *testing.T) {
	if _, ok := LookupProvider("unknown-provider"); ok {
		t.Error("LookupProvider(\"unknown-provider\") returned true")
	}
}

func TestListProviders(t *testing.T) {
	names := ListProviders()
	if len(names) == 0 {
		t.Fatal("ListProviders() returned empty slice")
	}
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Errorf("ListProviders() not sorted: %q before %q", names[i-1], names[i])
		}
	}
	found := false
	for _, n := range names {
		if n == "anthropic" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ListProviders() does not include \"anthropic\"")
	}
}
