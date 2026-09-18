package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos/storage"
)

type tenantContextKey struct{}
type correlationIDContextKey struct{}

const correlationIDHeader = "X-Request-ID"

// CorrelationIDFromContext returns the request correlation ID assigned by the
// HTTP middleware.
func CorrelationIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(correlationIDContextKey{}).(string)
	return id, ok && id != ""
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(correlationIDHeader))
		if !validCorrelationID(id) {
			var value [16]byte
			if _, err := rand.Read(value[:]); err != nil {
				id = fmt.Sprintf("%d", time.Now().UnixNano())
			} else {
				id = hex.EncodeToString(value[:])
			}
		}
		w.Header().Set(correlationIDHeader, id)
		ctx := context.WithValue(r.Context(), correlationIDContextKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func validCorrelationID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '-' && char != '_' && char != '.' {
			return false
		}
	}
	return true
}

func requestDeadlineMiddleware(timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func maxBodyMiddleware(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// TenantIDFromContext returns the tenant authenticated for this request.
// Tenant IDs are only added by authentication middleware, never from headers
// or request parameters.
func TenantIDFromContext(ctx context.Context) (string, bool) {
	tenantID, ok := ctx.Value(tenantContextKey{}).(string)
	return tenantID, ok && tenantID != ""
}

// tenantMiddleware scopes authenticated requests to their validated tenant.
func tenantMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if probePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		tenantID, ok := TenantIDFromContext(r.Context())
		if !ok {
			http.Error(w, `{"error":"tenant identity is missing"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(storage.WithTenant(r.Context(), tenantID)))
	})
}

// authMiddleware returns middleware that enforces API-key authentication.
// When authType is "none", all requests pass through. When authType is
// "api_key", requests must include an Authorization: Bearer <key> header
// matching apiKey. tenantID is bound to a successfully validated API key.
// Health and ready probes are always exempt.
func authMiddleware(authType, apiKey, tenantID string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if authType == "none" || probePath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			header := r.Header.Get("Authorization")
			if !strings.HasPrefix(header, "Bearer ") {
				http.Error(w, `{"error":"missing or invalid Authorization header"}`, http.StatusUnauthorized)
				return
			}
			token := strings.TrimPrefix(header, "Bearer ")
			if token != apiKey {
				http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
				return
			}
			if strings.TrimSpace(tenantID) == "" {
				http.Error(w, `{"error":"tenant is not configured"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantContextKey{}, tenantID)))
		})
	}
}

// corsMiddleware returns middleware that sets CORS headers. origins is a
// comma-separated list of allowed origins; "*" allows all. OPTIONS preflight
// requests are handled and short-circuited.
func corsMiddleware(origins string) func(http.Handler) http.Handler {
	if origins == "" {
		origins = "*"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			if origins == "*" || containsOrigin(origins, origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-ID")
			w.Header().Set("Access-Control-Max-Age", "86400")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// oidcAuthMiddleware returns middleware that validates Bearer tokens as OIDC
// JWTs using the given OIDCValidator. Health and ready probes are exempt.
func oidcAuthMiddleware(validator *auth.OIDCValidator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if probePath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			header := r.Header.Get("Authorization")
			if !strings.HasPrefix(header, "Bearer ") {
				http.Error(w, `{"error":"missing or invalid Authorization header"}`, http.StatusUnauthorized)
				return
			}
			token := strings.TrimPrefix(header, "Bearer ")
			claims, err := validator.Validate(token)
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusUnauthorized)
				return
			}
			if strings.TrimSpace(claims.TenantID) == "" {
				http.Error(w, `{"error":"OIDC token is missing tenant identity"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantContextKey{}, claims.TenantID)))
		})
	}
}

func probePath(path string) bool {
	return path == "/health" || path == "/live" || path == "/ready" || path == "/draining"
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}
func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		wrapped := &statusWriter{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				wrapped.status = http.StatusInternalServerError
				s.logRequest(r, wrapped.status, started)
				panic(recovered)
			}
			s.logRequest(r, wrapped.status, started)
		}()
		next.ServeHTTP(wrapped, r)
	})
}

func (s *Server) logRequest(r *http.Request, status int, started time.Time) {
	if status == 0 {
		status = http.StatusOK
	}
	correlationID, _ := CorrelationIDFromContext(r.Context())
	tenantID, _ := TenantIDFromContext(r.Context())
	s.logger.Log(r.Context(), slog.LevelInfo, "http_request",
		"correlation_id", correlationID, "tenant_id", tenantID, "method", r.Method,
		"route", r.URL.Path, "status", status, "duration_ms", time.Since(started).Milliseconds())
}

func containsOrigin(allowed, origin string) bool {
	for _, o := range strings.Split(allowed, ",") {
		if strings.TrimSpace(o) == origin {
			return true
		}
	}
	return false
}

// rateLimiter enforces per-IP request rate limits using a simple token-bucket
// approach: each client gets limit tokens per window, replenished on first
// request after the window elapses.
type rateLimiter struct {
	mu      sync.Mutex
	clients map[string]*clientBucket
	limit   int
	window  time.Duration
}

type clientBucket struct {
	tokens    int
	lastReset time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		clients: make(map[string]*clientBucket),
		limit:   limit,
		window:  window,
	}
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.clients[ip]
	if !ok || now.Sub(b.lastReset) >= rl.window {
		rl.clients[ip] = &clientBucket{tokens: rl.limit - 1, lastReset: now}
		return true
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

func (rl *rateLimiter) cleanup(now time.Time, limit int) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	removed := 0
	for ip, bucket := range rl.clients {
		if removed >= limit {
			break
		}
		if now.Sub(bucket.lastReset) >= rl.window {
			delete(rl.clients, ip)
			removed++
		}
	}
	return removed
}

func rateLimitMiddleware(limiter *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if !limiter.allow(ip) {
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.SplitN(fwd, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
