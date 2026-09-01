package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// withFakeHome points os.UserHomeDir (via $HOME) at a fresh temp directory
// for the duration of the test, isolating LoadClaudeCodeCredential and
// LoadCodexCredential from whatever real Claude Code / Codex CLI state
// happens to exist on the machine running the test.
func withFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestLoadClaudeCodeCredentialMissingFile(t *testing.T) {
	withFakeHome(t)
	if _, err := LoadClaudeCodeCredential(); err != ErrNoExternalCredential {
		t.Fatalf("err = %v, want ErrNoExternalCredential", err)
	}
}

func TestLoadClaudeCodeCredentialParsesAccessToken(t *testing.T) {
	home := withFakeHome(t)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-abc","refreshToken":"sk-ant-ort01-def","expiresAt":1748658860401,"scopes":["user:inference","user:profile"]}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cred, err := LoadClaudeCodeCredential()
	if err != nil {
		t.Fatalf("LoadClaudeCodeCredential: %v", err)
	}
	if cred.AccessToken != "sk-ant-oat01-abc" {
		t.Errorf("AccessToken = %q, want sk-ant-oat01-abc", cred.AccessToken)
	}
	if cred.RefreshToken != "sk-ant-ort01-def" {
		t.Errorf("RefreshToken = %q, want sk-ant-ort01-def", cred.RefreshToken)
	}
	if cred.ExpiresAt.UnixMilli() != 1748658860401 {
		t.Errorf("ExpiresAt = %v, want unix millis 1748658860401", cred.ExpiresAt)
	}
	if cred.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", cred.Provider)
	}
}

func TestLoadClaudeCodeCredentialEmptyAccessTokenIsNotFound(t *testing.T) {
	home := withFakeHome(t)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClaudeCodeCredential(); err != ErrNoExternalCredential {
		t.Fatalf("err = %v, want ErrNoExternalCredential", err)
	}
}

func TestLoadCodexCredentialMissingFile(t *testing.T) {
	withFakeHome(t)
	if _, err := LoadCodexCredential(); err != ErrNoExternalCredential {
		t.Fatalf("err = %v, want ErrNoExternalCredential", err)
	}
}

func TestLoadCodexCredentialPrefersOAuthTokensOverAPIKey(t *testing.T) {
	home := withFakeHome(t)
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"auth_mode":"chatgpt","OPENAI_API_KEY":"sk-leftover","tokens":{"access_token":"chatgpt-access","refresh_token":"chatgpt-refresh","account_id":"acct-1"}}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cred, err := LoadCodexCredential()
	if err != nil {
		t.Fatalf("LoadCodexCredential: %v", err)
	}
	if cred.AccessToken != "chatgpt-access" {
		t.Errorf("AccessToken = %q, want chatgpt-access", cred.AccessToken)
	}
	if cred.Method != MethodOAuthPKCE {
		t.Errorf("Method = %q, want oauth_pkce", cred.Method)
	}
}

func TestLoadCodexCredentialFallsBackToAPIKey(t *testing.T) {
	home := withFakeHome(t)
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"auth_mode":"apiKey","OPENAI_API_KEY":"sk-plain"}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cred, err := LoadCodexCredential()
	if err != nil {
		t.Fatalf("LoadCodexCredential: %v", err)
	}
	if cred.APIKey != "sk-plain" {
		t.Errorf("APIKey = %q, want sk-plain", cred.APIKey)
	}
	if cred.Method != MethodAPIKey {
		t.Errorf("Method = %q, want api_key", cred.Method)
	}
}
