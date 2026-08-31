package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	handler := applyMiddleware(echoHandler(), "api_key", "secret-key")
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
	handler := applyMiddleware(echoHandler(), "api_key", "secret-key")
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
	handler := applyMiddleware(echoHandler(), "api_key", "secret-key")
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

func TestAuthMiddleware_NonePassesThrough(t *testing.T) {
	handler := applyMiddleware(echoHandler(), "none", "")
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
	handler := applyMiddleware(echoHandler(), "api_key", "secret-key")
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
	handler = authMiddleware(cfg.AuthType, cfg.APIKey)(handler)
	handler = corsMiddleware(cfg.CORSOrigins)(handler)
	return handler
}

func applyMiddleware(h http.Handler, authType, apiKey string) http.Handler {
	// Wrap with a mux so path-based auth exemptions work.
	mux := http.NewServeMux()
	mux.Handle("/health", h)
	mux.Handle("/ready", h)
	mux.Handle("/v1/", h)
	mux.Handle("/", h)
	return authMiddleware(authType, apiKey)(mux)
}

func echoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"echo": "ok"})
	})
}
