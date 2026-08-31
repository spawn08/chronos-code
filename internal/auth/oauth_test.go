package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestPKCEVerifierChallengeIsValidS256Pair(t *testing.T) {
	verifier := oauth2.GenerateVerifier()
	challenge := oauth2.S256ChallengeFromVerifier(verifier)

	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])

	if challenge != want {
		t.Fatalf("challenge = %q, want %q (recomputed S256 of verifier)", challenge, want)
	}
	if verifier == "" {
		t.Fatal("verifier is empty")
	}
}

func TestBuildAuthURL(t *testing.T) {
	cfg := ProviderOAuthConfig{
		Provider:     "acme",
		ClientID:     "client-123",
		AuthURL:      "https://idp.example.com/authorize",
		TokenURL:     "https://idp.example.com/token",
		Scopes:       []string{"openid", "profile"},
		RedirectPort: 8765,
	}
	state := "test-state"
	challenge := "test-challenge"

	got := buildAuthURL(cfg, state, challenge)

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
	if q.Get("code_challenge") != challenge {
		t.Errorf("code_challenge = %q, want %q", q.Get("code_challenge"), challenge)
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("state") != state {
		t.Errorf("state = %q, want %q", q.Get("state"), state)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d/callback", cfg.RedirectPort); q.Get("redirect_uri") != want {
		t.Errorf("redirect_uri = %q, want %q", q.Get("redirect_uri"), want)
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code", q.Get("response_type"))
	}
	gotScopes := strings.Fields(q.Get("scope"))
	if len(gotScopes) != 2 || gotScopes[0] != "openid" || gotScopes[1] != "profile" {
		t.Errorf("scope = %q, want %v", q.Get("scope"), cfg.Scopes)
	}
}

// deviceCodeTokenServer fakes an RFC 8628 device-authorization + token
// endpoint pair. It returns authorization_pending on the first token poll
// and a successful token response on the second, unless configured
// otherwise via errorCode.
type deviceCodeTokenServer struct {
	pollCount int
	errorCode string // if set, every token poll returns this error instead of succeeding.
}

func newDeviceCodeTestServer(t *testing.T, s *deviceCodeTokenServer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "devcode-123",
			"user_code":        "ABCD-EFGH",
			"verification_uri": "https://idp.example.com/device",
			"interval":         1, // short poll interval so the test doesn't wait on oauth2's 5s default.
			"expires_in":       300,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		s.pollCount++
		w.Header().Set("Content-Type", "application/json")
		if s.errorCode != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": s.errorCode})
			return
		}
		if s.pollCount == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-tok",
			"refresh_token": "refresh-tok",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestLoginDeviceCodeSucceedsAfterPending(t *testing.T) {
	fake := &deviceCodeTokenServer{}
	ts := newDeviceCodeTestServer(t, fake)

	store := NewStoreWithBackend(newFakeKeyringBackend(), filepath.Join(t.TempDir(), "providers.json"))
	cfg := ProviderOAuthConfig{
		Provider: "acme",
		ClientID: "client-123",
		AuthURL:  ts.URL + "/device",
		TokenURL: ts.URL + "/token",
		Scopes:   []string{"openid"},
	}

	var promptedUser, promptedURI string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := LoginDeviceCode(ctx, store, cfg, func(userCode, verificationURI string) {
		promptedUser = userCode
		promptedURI = verificationURI
	})
	if err != nil {
		t.Fatalf("LoginDeviceCode: %v", err)
	}
	if promptedUser != "ABCD-EFGH" || promptedURI != "https://idp.example.com/device" {
		t.Errorf("onPrompt got (%q, %q)", promptedUser, promptedURI)
	}

	cred, err := store.Load("acme")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cred.Method != MethodDeviceCode || cred.AccessToken != "access-tok" || cred.RefreshToken != "refresh-tok" {
		t.Fatalf("Load = %+v, want device-code credential with access-tok/refresh-tok", cred)
	}
	if cred.ExpiresAt.Before(time.Now().Add(time.Hour - time.Minute)) {
		t.Errorf("ExpiresAt = %v, want ~1h from now", cred.ExpiresAt)
	}
	if fake.pollCount < 2 {
		t.Errorf("pollCount = %d, want at least 2 (pending then success)", fake.pollCount)
	}
}

func TestLoginDeviceCodeExpiredToken(t *testing.T) {
	fake := &deviceCodeTokenServer{errorCode: "expired_token"}
	ts := newDeviceCodeTestServer(t, fake)

	store := NewStoreWithBackend(newFakeKeyringBackend(), filepath.Join(t.TempDir(), "providers.json"))
	cfg := ProviderOAuthConfig{
		Provider: "acme",
		ClientID: "client-123",
		AuthURL:  ts.URL + "/device",
		TokenURL: ts.URL + "/token",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := LoginDeviceCode(ctx, store, cfg, nil)
	if err == nil {
		t.Fatal("LoginDeviceCode with expired_token response should fail")
	}
	if !strings.Contains(err.Error(), "expired_token") {
		t.Fatalf("error = %v, want it to mention expired_token", err)
	}
}

func TestLoginDeviceCodeAccessDenied(t *testing.T) {
	fake := &deviceCodeTokenServer{errorCode: "access_denied"}
	ts := newDeviceCodeTestServer(t, fake)

	store := NewStoreWithBackend(newFakeKeyringBackend(), filepath.Join(t.TempDir(), "providers.json"))
	cfg := ProviderOAuthConfig{
		Provider: "acme",
		ClientID: "client-123",
		AuthURL:  ts.URL + "/device",
		TokenURL: ts.URL + "/token",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := LoginDeviceCode(ctx, store, cfg, nil)
	if err == nil {
		t.Fatal("LoginDeviceCode with access_denied response should fail")
	}
	if !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("error = %v, want it to mention access_denied", err)
	}
}

// refreshTokenServer fakes a token endpoint that only supports
// grant_type=refresh_token, returning a fresh access token each call.
func newRefreshTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != "refresh_token" {
			http.Error(w, "unexpected grant_type", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access-tok",
			"refresh_token": "new-refresh-tok",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestRefreshUpdatesStoredCredential(t *testing.T) {
	ts := newRefreshTestServer(t)
	store := NewStoreWithBackend(newFakeKeyringBackend(), filepath.Join(t.TempDir(), "providers.json"))
	cfg := ProviderOAuthConfig{
		Provider: "acme",
		ClientID: "client-123",
		TokenURL: ts.URL + "/token",
	}

	if err := store.Save("acme", Credential{
		Provider:     "acme",
		Method:       MethodOAuthPKCE,
		AccessToken:  "old-access-tok",
		RefreshToken: "old-refresh-tok",
		ExpiresAt:    time.Now().Add(-time.Minute), // already expired
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := Refresh(context.Background(), store, cfg); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	cred, err := store.Load("acme")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cred.AccessToken != "new-access-tok" || cred.RefreshToken != "new-refresh-tok" {
		t.Fatalf("Load after Refresh = %+v, want new tokens", cred)
	}
	if !cred.ExpiresAt.After(time.Now()) {
		t.Fatalf("ExpiresAt after Refresh = %v, want in the future", cred.ExpiresAt)
	}
	if cred.Method != MethodOAuthPKCE {
		t.Fatalf("Method after Refresh = %v, want unchanged MethodOAuthPKCE", cred.Method)
	}
}

func TestRefreshNoOpForAPIKey(t *testing.T) {
	store := NewStoreWithBackend(newFakeKeyringBackend(), filepath.Join(t.TempDir(), "providers.json"))
	if err := LoginAPIKey(store, "acme", "sk-abc"); err != nil {
		t.Fatalf("LoginAPIKey: %v", err)
	}
	cfg := ProviderOAuthConfig{Provider: "acme", TokenURL: "http://unused.invalid/token"}
	if err := Refresh(context.Background(), store, cfg); err != nil {
		t.Fatalf("Refresh on API key credential should be a no-op, got %v", err)
	}
}

func TestRefreshNoRefreshTokenErrors(t *testing.T) {
	store := NewStoreWithBackend(newFakeKeyringBackend(), filepath.Join(t.TempDir(), "providers.json"))
	if err := store.Save("acme", Credential{Provider: "acme", Method: MethodOAuthPKCE, AccessToken: "tok"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cfg := ProviderOAuthConfig{Provider: "acme", TokenURL: "http://unused.invalid/token"}
	if err := Refresh(context.Background(), store, cfg); err == nil {
		t.Fatal("Refresh with no refresh token should error")
	}
}

func TestAutoRefreshIfNeededNoOpWhenFarFromExpiry(t *testing.T) {
	ts := newRefreshTestServer(t)
	store := NewStoreWithBackend(newFakeKeyringBackend(), filepath.Join(t.TempDir(), "providers.json"))
	cfg := ProviderOAuthConfig{Provider: "acme", ClientID: "client-123", TokenURL: ts.URL + "/token"}

	if err := store.Save("acme", Credential{
		Provider:     "acme",
		Method:       MethodOAuthPKCE,
		AccessToken:  "access-tok",
		RefreshToken: "refresh-tok",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := AutoRefreshIfNeeded(context.Background(), store, cfg, 5*time.Minute); err != nil {
		t.Fatalf("AutoRefreshIfNeeded: %v", err)
	}

	cred, err := store.Load("acme")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cred.AccessToken != "access-tok" {
		t.Fatalf("AccessToken changed to %q, want unchanged (no refresh should have happened)", cred.AccessToken)
	}
}

func TestAutoRefreshIfNeededTriggersWithinWindow(t *testing.T) {
	ts := newRefreshTestServer(t)
	store := NewStoreWithBackend(newFakeKeyringBackend(), filepath.Join(t.TempDir(), "providers.json"))
	cfg := ProviderOAuthConfig{Provider: "acme", ClientID: "client-123", TokenURL: ts.URL + "/token"}

	if err := store.Save("acme", Credential{
		Provider:     "acme",
		Method:       MethodOAuthPKCE,
		AccessToken:  "access-tok",
		RefreshToken: "refresh-tok",
		ExpiresAt:    time.Now().Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := AutoRefreshIfNeeded(context.Background(), store, cfg, 5*time.Minute); err != nil {
		t.Fatalf("AutoRefreshIfNeeded: %v", err)
	}

	cred, err := store.Load("acme")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cred.AccessToken != "new-access-tok" {
		t.Fatalf("AccessToken = %q, want refreshed new-access-tok", cred.AccessToken)
	}
}

func TestAutoRefreshIfNeededNoOpWhenNotAuthenticated(t *testing.T) {
	store := NewStoreWithBackend(newFakeKeyringBackend(), filepath.Join(t.TempDir(), "providers.json"))
	cfg := ProviderOAuthConfig{Provider: "acme", TokenURL: "http://unused.invalid/token"}
	if err := AutoRefreshIfNeeded(context.Background(), store, cfg, 5*time.Minute); err != nil {
		t.Fatalf("AutoRefreshIfNeeded for never-logged-in provider: %v", err)
	}
}
