package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestMetricsEndpointAndStructuredLogsExcludeRequestBody(t *testing.T) {
	var logs bytes.Buffer
	s := New(nil, ServerConfig{AuthType: "none", Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(`{"message":"do-not-log-this"}`))
	request.Header.Set(correlationIDHeader, "request-123")
	s.Handler().ServeHTTP(recorder, request)
	if !strings.Contains(logs.String(), `"correlation_id":"request-123"`) {
		t.Fatalf("log missing correlation: %s", logs.String())
	}
	if strings.Contains(logs.String(), "do-not-log-this") {
		t.Fatalf("log disclosed request body: %s", logs.String())
	}

	metrics := httptest.NewRecorder()
	s.Handler().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), "chronos_code_work_active") {
		t.Fatalf("metrics status=%d body=%q", metrics.Code, metrics.Body.String())
	}
}

func TestRequestDeadlineDefaultsAndPropagates(t *testing.T) {
	s := New(nil, ServerConfig{AuthType: "none"})
	if s.cfg.RequestTimeout != defaultRequestTimeout {
		t.Fatalf("RequestTimeout=%s, want %s", s.cfg.RequestTimeout, defaultRequestTimeout)
	}

	want := 30 * time.Second
	handler := requestDeadlineMiddleware(want)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("request context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > want {
			t.Fatalf("deadline remaining=%s, want within (0, %s]", remaining, want)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusNoContent)
	}
}

func TestJSONRequestsRejectUnknownFieldsAndTrailingValues(t *testing.T) {
	handler := New(nil, ServerConfig{AuthType: "none"}).Handler()
	for _, body := range []string{
		`{"message":"hello","unexpected":true}`,
		`{"message":"hello"} {"message":"again"}`,
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(body))
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body=%q status=%d, want %d", body, recorder.Code, http.StatusBadRequest)
		}
	}
}

func TestJSONRequestBodyLimit(t *testing.T) {
	handler := New(nil, ServerConfig{AuthType: "none"}).Handler()
	body := `{"message":"` + strings.Repeat("x", maxRequestBodyBytes) + `"}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(body))

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}
	if !strings.Contains(recorder.Body.String(), "request body too large") {
		t.Fatalf("body=%q, want body limit error", recorder.Body.String())
	}
}

func TestCorrelationIDAcceptedGeneratedAndPropagated(t *testing.T) {
	handler := correlationIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := CorrelationIDFromContext(r.Context())
		if !ok {
			t.Fatal("correlation ID missing from context")
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	}))

	accepted := httptest.NewRecorder()
	acceptedRequest := httptest.NewRequest(http.MethodGet, "/health", nil)
	acceptedRequest.Header.Set(correlationIDHeader, "caller-id")
	handler.ServeHTTP(accepted, acceptedRequest)
	if got := accepted.Header().Get(correlationIDHeader); got != "caller-id" {
		t.Fatalf("accepted correlation ID=%q, want caller-id", got)
	}
	if !strings.Contains(accepted.Body.String(), `"id":"caller-id"`) {
		t.Fatalf("body=%q, want propagated caller-id", accepted.Body.String())
	}

	generated := httptest.NewRecorder()
	handler.ServeHTTP(generated, httptest.NewRequest(http.MethodGet, "/health", nil))
	if got := generated.Header().Get(correlationIDHeader); got == "" {
		t.Fatal("generated correlation ID is empty")
	}
}

func TestCorrelationIDRejectsUnsafeCallerValue(t *testing.T) {
	handler := correlationIDMiddleware(echoHandler())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	request.Header.Set(correlationIDHeader, "unsafe value")
	handler.ServeHTTP(recorder, request)
	if got := recorder.Header().Get(correlationIDHeader); got == "" || got == "unsafe value" {
		t.Fatalf("generated correlation ID=%q", got)
	}
}

func TestRecoveryMiddlewareReturnsStructuredJSON(t *testing.T) {
	handler := recoveryMiddleware(correlationIDMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panic", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", recorder.Header().Get("Content-Type"))
	}
	if recorder.Header().Get(correlationIDHeader) == "" {
		t.Fatal("panic response is missing correlation ID")
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil || payload["error"] != "internal server error" {
		t.Fatalf("panic response=%q, want structured internal error", recorder.Body.String())
	}
}

func TestDrainingKeepsHealthLiveAndFailsReadiness(t *testing.T) {
	s := New(nil, ServerConfig{AuthType: "none"})
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if !s.draining.Load() {
		t.Fatal("Shutdown() did not mark the server draining")
	}

	health := httptest.NewRecorder()
	s.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status=%d, want %d", health.Code, http.StatusOK)
	}
	ready := httptest.NewRecorder()
	s.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if ready.Code != http.StatusServiceUnavailable || !strings.Contains(ready.Body.String(), "draining") {
		t.Fatalf("ready status=%d body=%q, want draining 503", ready.Code, ready.Body.String())
	}
}

func TestDrainingRejectsNewExecution(t *testing.T) {
	s := New(nil, ServerConfig{AuthType: "none"})
	s.draining.Store(true)
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(`{"message":"hello"}`)))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d retry-after=%q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
}

func TestFleetRejectsRequestOnNonOwnerBeforeExecution(t *testing.T) {
	s := New(nil, ServerConfig{AuthType: "none", InstanceID: "node-a", FleetInstances: []string{"node-a", "node-b"}})
	sessionID := "sess-a"
	for s.router.IsLocal(sessionID) {
		sessionID += "x"
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(`{"message":"secret prompt","session_id":"`+sessionID+`"}`))
	s.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusConflict)
	}
	if got, want := recorder.Header().Get("X-Chronos-Session-Owner"), s.router.Owner(sessionID); got != want {
		t.Fatalf("owner=%q, want %q", got, want)
	}
}

func TestFleetConfigurationRequiresNamedMember(t *testing.T) {
	for _, cfg := range []ServerConfig{
		{AuthType: "none", FleetInstances: []string{"node-a", "node-b"}},
		{AuthType: "none", InstanceID: "node-c", FleetInstances: []string{"node-a", "node-b"}},
	} {
		if New(nil, cfg).configErr == nil {
			t.Fatalf("config %+v unexpectedly accepted", cfg)
		}
	}
}

func TestMaxConcurrencyDefaultsToOne(t *testing.T) {
	s := New(nil, ServerConfig{AuthType: "none"})
	if s.cfg.MaxConcurrent != 1 || cap(s.executions) != 1 {
		t.Fatalf("MaxConcurrent=%d capacity=%d, want 1", s.cfg.MaxConcurrent, cap(s.executions))
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

func TestExecutionConcurrencyLimit(t *testing.T) {
	s := New(nil, ServerConfig{AuthType: "none", MaxConcurrent: 1})
	started := make(chan struct{})
	release := make(chan struct{})
	handler := s.limitExecution(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat", nil))
		firstDone <- response
	}()
	<-started

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/chat", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status=%d, want %d", second.Code, http.StatusTooManyRequests)
	}
	if second.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After=%q, want 1", second.Header().Get("Retry-After"))
	}

	close(release)
	if first := <-firstDone; first.Code != http.StatusNoContent {
		t.Fatalf("first status=%d, want %d", first.Code, http.StatusNoContent)
	}
}

func TestIdempotencyKeyPreventsDuplicateExecution(t *testing.T) {
	s := New(nil, ServerConfig{AuthType: "none"})
	var calls atomic.Int32
	handler := s.idempotentExecution(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat", nil)
		req.Header.Set("Idempotency-Key", "same-task")
		return req
	}
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request())
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, request())
	if first.Code != http.StatusNoContent || second.Code != http.StatusConflict || calls.Load() != 1 {
		t.Fatalf("statuses=(%d,%d) calls=%d", first.Code, second.Code, calls.Load())
	}
}

func TestIdempotencyKeyIsTenantScoped(t *testing.T) {
	s := New(nil, ServerConfig{AuthType: "none"})
	var calls atomic.Int32
	handler := s.idempotentExecution(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat", nil)
		req.Header.Set("Idempotency-Key", "same-task")
		req = req.WithContext(context.WithValue(req.Context(), tenantContextKey{}, tenant))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	if calls.Load() != 2 {
		t.Fatalf("tenant-scoped calls = %d, want 2", calls.Load())
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
