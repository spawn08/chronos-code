package auth

import (
	"net/url"
	"testing"
)

func TestOpenAICodexSubscriptionConfigProducesExpectedAuthURL(t *testing.T) {
	cfg := OpenAICodexSubscriptionConfig()
	if cfg.Provider != "openai" {
		t.Fatalf("Provider = %q, want openai", cfg.Provider)
	}
	if cfg.redirectURI() != "http://127.0.0.1:1455/auth/callback" {
		t.Fatalf("redirectURI() = %q, want the fixed Codex callback path on port 1455", cfg.redirectURI())
	}

	got := buildAuthURL(cfg, "test-state", "test-challenge")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("buildAuthURL produced invalid URL: %v", err)
	}
	if u.Scheme+"://"+u.Host+u.Path != cfg.AuthURL {
		t.Fatalf("base URL = %q, want %q", u.Scheme+"://"+u.Host+u.Path, cfg.AuthURL)
	}

	q := u.Query()
	if q.Get("client_id") != cfg.ClientID {
		t.Errorf("client_id = %q, want %q", q.Get("client_id"), cfg.ClientID)
	}
	if q.Get("redirect_uri") != "http://127.0.0.1:1455/auth/callback" {
		t.Errorf("redirect_uri = %q, want the fixed callback URL", q.Get("redirect_uri"))
	}
	for k, want := range cfg.ExtraAuthParams {
		if got := q.Get(k); got != want {
			t.Errorf("query param %q = %q, want %q", k, got, want)
		}
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
}
