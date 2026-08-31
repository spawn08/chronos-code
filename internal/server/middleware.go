package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/spawn08/chronos-code/internal/auth"
)

// authMiddleware returns middleware that enforces API-key authentication.
// When authType is "none", all requests pass through. When authType is
// "api_key", requests must include an Authorization: Bearer <key> header
// matching apiKey. Health and ready probes are always exempt.
func authMiddleware(authType, apiKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if authType == "none" || r.URL.Path == "/health" || r.URL.Path == "/ready" {
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
			next.ServeHTTP(w, r)
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
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
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
			if r.URL.Path == "/health" || r.URL.Path == "/ready" {
				next.ServeHTTP(w, r)
				return
			}
			header := r.Header.Get("Authorization")
			if !strings.HasPrefix(header, "Bearer ") {
				http.Error(w, `{"error":"missing or invalid Authorization header"}`, http.StatusUnauthorized)
				return
			}
			token := strings.TrimPrefix(header, "Bearer ")
			_, err := validator.Validate(token)
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
