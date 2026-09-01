package auth

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// clearAuthEnv resets every env var any precedence chain reads to "" for the
// duration of the test, so a real ANTHROPIC_API_KEY etc. set in the
// developer's shell can never leak into these tests.
func clearAuthEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
		"CODEX_ACCESS_TOKEN", "OPENAI_API_KEY",
	} {
		t.Setenv(k, "")
	}
}

func TestResolveAnthropicPrecedence(t *testing.T) {
	ctx := context.Background()

	t.Run("gateway token wins over everything", func(t *testing.T) {
		clearAuthEnv(t)
		withFakeHome(t)
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "gw-token")
		t.Setenv("ANTHROPIC_API_KEY", "api-key")
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-env-token")
		store := newTestStore(t)

		rc := Resolve(ctx, store, "anthropic")
		if rc.Token != "gw-token" || rc.Source != SourceGatewayEnv {
			t.Fatalf("rc = %+v, want gw-token via SourceGatewayEnv", rc)
		}
	})

	t.Run("api key env wins over oauth env and store", func(t *testing.T) {
		clearAuthEnv(t)
		withFakeHome(t)
		t.Setenv("ANTHROPIC_API_KEY", "api-key")
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-env-token")
		store := newTestStore(t)

		rc := Resolve(ctx, store, "anthropic")
		if rc.Token != "api-key" || rc.Source != SourceAPIKeyEnv {
			t.Fatalf("rc = %+v, want api-key via SourceAPIKeyEnv", rc)
		}
	})

	t.Run("long-lived oauth env wins over store and reuse", func(t *testing.T) {
		clearAuthEnv(t)
		withFakeHome(t)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-env-token")
		store := newTestStore(t)

		rc := Resolve(ctx, store, "anthropic")
		if rc.Token != "oauth-env-token" || rc.Source != SourceLongLivedEnv {
			t.Fatalf("rc = %+v, want oauth-env-token via SourceLongLivedEnv", rc)
		}
	})

	t.Run("own stored oauth credential wins over external reuse", func(t *testing.T) {
		clearAuthEnv(t)
		withFakeHome(t)
		store := newTestStore(t)
		if err := store.Save("anthropic", Credential{
			Provider:    "anthropic",
			Method:      MethodOAuthPKCE,
			AccessToken: "own-oauth-token",
			ExpiresAt:   time.Now().Add(24 * time.Hour), // far from DefaultRefreshWindow; no refresh attempted
		}); err != nil {
			t.Fatal(err)
		}

		rc := Resolve(ctx, store, "anthropic")
		if rc.Token != "own-oauth-token" || rc.Source != SourceOwnOAuth {
			t.Fatalf("rc = %+v, want own-oauth-token via SourceOwnOAuth", rc)
		}
	})

	t.Run("external Claude Code credential reuse wins over own api key", func(t *testing.T) {
		clearAuthEnv(t)
		home := withFakeHome(t)
		dir := filepath.Join(home, ".claude")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"claudeAiOauth":{"accessToken":"claude-code-token","expiresAt":` + strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10) + `}}`
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		store := newTestStore(t)
		if err := store.Save("anthropic", Credential{Provider: "anthropic", Method: MethodAPIKey, APIKey: "own-key"}); err != nil {
			t.Fatal(err)
		}

		rc := Resolve(ctx, store, "anthropic")
		if rc.Token != "claude-code-token" || rc.Source != SourceExternalReuse {
			t.Fatalf("rc = %+v, want claude-code-token via SourceExternalReuse", rc)
		}
	})

	t.Run("own stored api key is the last resort", func(t *testing.T) {
		clearAuthEnv(t)
		withFakeHome(t)
		store := newTestStore(t)
		if err := store.Save("anthropic", Credential{Provider: "anthropic", Method: MethodAPIKey, APIKey: "own-key"}); err != nil {
			t.Fatal(err)
		}

		rc := Resolve(ctx, store, "anthropic")
		if rc.Token != "own-key" || rc.Source != SourceOwnAPIKey {
			t.Fatalf("rc = %+v, want own-key via SourceOwnAPIKey", rc)
		}
	})

	t.Run("nothing resolves to SourceNone", func(t *testing.T) {
		clearAuthEnv(t)
		withFakeHome(t)
		store := newTestStore(t)

		rc := Resolve(ctx, store, "anthropic")
		if rc.Source != SourceNone || rc.Token != "" {
			t.Fatalf("rc = %+v, want empty SourceNone", rc)
		}
	})
}

func TestResolveOpenAIPrecedence(t *testing.T) {
	ctx := context.Background()

	t.Run("codex access token env wins over api key env", func(t *testing.T) {
		clearAuthEnv(t)
		withFakeHome(t)
		t.Setenv("CODEX_ACCESS_TOKEN", "codex-gw-token")
		t.Setenv("OPENAI_API_KEY", "api-key")
		store := newTestStore(t)

		rc := Resolve(ctx, store, "openai")
		if rc.Token != "codex-gw-token" || rc.Source != SourceGatewayEnv {
			t.Fatalf("rc = %+v, want codex-gw-token via SourceGatewayEnv", rc)
		}
	})

	t.Run("openai api key env wins over reuse", func(t *testing.T) {
		clearAuthEnv(t)
		home := withFakeHome(t)
		dir := filepath.Join(home, ".codex")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"OPENAI_API_KEY":"codex-file-key"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OPENAI_API_KEY", "env-key")
		store := newTestStore(t)

		rc := Resolve(ctx, store, "openai")
		if rc.Token != "env-key" || rc.Source != SourceAPIKeyEnv {
			t.Fatalf("rc = %+v, want env-key via SourceAPIKeyEnv", rc)
		}
	})

	t.Run("codex file reuse wins over own stored api key", func(t *testing.T) {
		clearAuthEnv(t)
		home := withFakeHome(t)
		dir := filepath.Join(home, ".codex")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"OPENAI_API_KEY":"codex-file-key"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		store := newTestStore(t)
		if err := store.Save("openai", Credential{Provider: "openai", Method: MethodAPIKey, APIKey: "own-key"}); err != nil {
			t.Fatal(err)
		}

		rc := Resolve(ctx, store, "openai")
		if rc.Token != "codex-file-key" || rc.Source != SourceExternalReuse {
			t.Fatalf("rc = %+v, want codex-file-key via SourceExternalReuse", rc)
		}
	})
}

func TestResolveGenericProviderUnaffectedByChains(t *testing.T) {
	clearAuthEnv(t)
	withFakeHome(t)
	store := newTestStore(t)
	if err := store.Save("gemini", Credential{Provider: "gemini", Method: MethodAPIKey, APIKey: "gemini-key"}); err != nil {
		t.Fatal(err)
	}

	rc := Resolve(context.Background(), store, "gemini")
	if rc.Token != "gemini-key" || rc.Source != SourceOwnAPIKey {
		t.Fatalf("rc = %+v, want gemini-key via SourceOwnAPIKey", rc)
	}
}
