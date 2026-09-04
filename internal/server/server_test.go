package server

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos/storage"
)

func TestHealthEndpoint(t *testing.T) {
	handler := buildTestHandler(ServerConfig{AuthType: "none"})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status=%d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("health body=%s, want ok", body)
	}
}

func TestAuthMiddleware_APIKeyRejectsNoHeader(t *testing.T) {
	handler := applyMiddleware(echoHandler(), "api_key", "secret-key", "tenant-a")
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAuthMiddleware_APIKeyRejectsWrongKey(t *testing.T) {
	handler := applyMiddleware(echoHandler(), "api_key", "secret-key", "tenant-a")
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAuthMiddleware_APIKeyAcceptsCorrectKey(t *testing.T) {
	handler := applyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID, ok := TenantIDFromContext(r.Context())
		if !ok || tenantID != "tenant-a" {
			http.Error(w, "trusted tenant missing", http.StatusInternalServerError)
			return
		}
		if tenantID := storage.TenantFromContext(r.Context()); tenantID != "tenant-a" {
			http.Error(w, "storage tenant missing", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"echo": "ok"})
	}), "api_key", "secret-key", "tenant-a")
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestTenantMiddlewareRejectsMissingTenant(t *testing.T) {
	handler := tenantMiddleware(echoHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", resp.Code, http.StatusUnauthorized)
	}
}

func TestTenantMiddlewarePreservesStreaming(t *testing.T) {
	handler := tenantMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tenantID := storage.TenantFromContext(r.Context()); tenantID != "tenant-a" {
			http.Error(w, "storage tenant missing", http.StatusInternalServerError)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/stream", nil)
	req = req.WithContext(context.WithValue(req.Context(), tenantContextKey{}, "tenant-a"))
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", resp.Code, http.StatusOK)
	}
}

func TestAuthMiddleware_APIKeyRejectsMissingTenant(t *testing.T) {
	handler := applyMiddleware(echoHandler(), "api_key", "secret-key", "")
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestServerRejectsAPIKeyConfigurationWithoutTenant(t *testing.T) {
	s := New(nil, ServerConfig{AuthType: "api_key", APIKey: "secret-key"})
	if s.configErr == nil {
		t.Fatal("expected missing API-key tenant configuration to fail")
	}
}

func TestOIDCRequestRejectsMissingTenantClaim(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": issuer.URL + "/jwks"})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "use": "sig", "kid": "test-key", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB",
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()

	validator, err := auth.NewOIDCValidator(auth.OIDCConfig{Issuer: issuer.URL, ClientID: "chronos-code"})
	if err != nil {
		t.Fatalf("NewOIDCValidator: %v", err)
	}
	token := signServerTestJWT(t, key, map[string]any{
		"sub": "user-1", "iss": issuer.URL, "aud": "chronos-code",
		"exp": time.Now().Add(time.Hour).Unix(), "scope": "openid",
	})
	handler := oidcAuthMiddleware(validator)(tenantMiddleware(echoHandler()))
	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", resp.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(resp.Body.String(), "OIDC token is missing tenant identity") {
		t.Fatalf("body=%q, want missing tenant identity error", resp.Body.String())
	}
}

func TestAuthMiddleware_NonePassesThrough(t *testing.T) {
	handler := applyMiddleware(echoHandler(), "none", "", "")
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestAuthMiddleware_HealthExempt(t *testing.T) {
	handler := applyMiddleware(echoHandler(), "api_key", "secret-key", "tenant-a")
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health should bypass auth, got %d", resp.StatusCode)
	}
}

func TestCORSHeaders(t *testing.T) {
	handler := corsMiddleware("https://app.example.com")(echoHandler())
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/agents", nil)
	req.Header.Set("Origin", "https://app.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("CORS origin=%q, want https://app.example.com", got)
	}
}

func TestCORSPreflight(t *testing.T) {
	handler := corsMiddleware("*")(echoHandler())
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest("OPTIONS", ts.URL+"/v1/chat", nil)
	req.Header.Set("Origin", "https://any.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status=%d, want 204", resp.StatusCode)
	}
}

func TestRateLimiter_BlocksAfterLimit(t *testing.T) {
	rl := newRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if rl.allow("1.2.3.4") {
		t.Fatal("4th request should be blocked")
	}
}

func TestRateLimiter_DifferentIPsIndependent(t *testing.T) {
	rl := newRateLimiter(1, time.Minute)
	if !rl.allow("1.1.1.1") {
		t.Fatal("first IP first request should be allowed")
	}
	if rl.allow("1.1.1.1") {
		t.Fatal("first IP second request should be blocked")
	}
	if !rl.allow("2.2.2.2") {
		t.Fatal("second IP first request should be allowed")
	}
}

func TestRateLimiter_ResetsAfterWindow(t *testing.T) {
	rl := newRateLimiter(1, 10*time.Millisecond)
	if !rl.allow("1.2.3.4") {
		t.Fatal("first request should pass")
	}
	if rl.allow("1.2.3.4") {
		t.Fatal("immediate second should be blocked")
	}
	time.Sleep(15 * time.Millisecond)
	if !rl.allow("1.2.3.4") {
		t.Fatal("request after window should be allowed")
	}
}

func TestContainsOrigin(t *testing.T) {
	if !containsOrigin("https://a.com, https://b.com", "https://b.com") {
		t.Fatal("should match b.com")
	}
	if containsOrigin("https://a.com", "https://evil.com") {
		t.Fatal("should not match evil.com")
	}
}

// --- helpers ---

// buildTestHandler creates a minimal handler that wires the health endpoint
// via a real mux, but has no orchestrator backing it. Only suitable for
// testing middleware + health.
func buildTestHandler(cfg ServerConfig) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	var handler http.Handler = mux
	handler = authMiddleware(cfg.AuthType, cfg.APIKey, cfg.TenantID)(handler)
	handler = corsMiddleware(cfg.CORSOrigins)(handler)
	return handler
}

func applyMiddleware(h http.Handler, authType, apiKey, tenantID string) http.Handler {
	// Wrap with a mux so path-based auth exemptions work.
	mux := http.NewServeMux()
	mux.Handle("/health", h)
	mux.Handle("/ready", h)
	mux.Handle("/v1/", h)
	mux.Handle("/", h)
	if authType == "api_key" {
		return authMiddleware(authType, apiKey, tenantID)(tenantMiddleware(mux))
	}
	return authMiddleware(authType, apiKey, tenantID)(mux)
}

func echoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"echo": "ok"})
	})
}

func signServerTestJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test-key","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal JWT claims: %v", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	hash := sha256.Sum256([]byte(header + "." + payloadB64))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return header + "." + payloadB64 + "." + base64.RawURLEncoding.EncodeToString(signature)
}
