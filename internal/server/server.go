// Package server implements PRD P3-004: an HTTP server that exposes the
// same agent capabilities as the CLI via a REST API with SSE streaming,
// authentication, rate limiting, and CORS support for team/enterprise
// deployment.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/observability"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/retention"
	"github.com/spawn08/chronos/engine/hooks"
)

// ServerConfig holds configuration for the HTTP server.
type ServerConfig struct {
	Listen          string // address to listen on, e.g. ":8430"
	AuthType        string // "api_key", "oidc", or "none"
	APIKey          string // required when AuthType is "api_key"
	TenantID        string // required when AuthType is "api_key"
	RepositoryID    string // trusted repository identity for all non-probe routes
	Authorizer      authorization.Authorizer
	DeliveryStore   *execution.DeliveryStore // durable admission/inspection; owned by the caller
	OIDCIssuer      string       // required when AuthType is "oidc"
	OIDCClientID    string       // required when AuthType is "oidc"
	CORSOrigins     string       // comma-separated allowed origins; "*" for all
	MaxConcurrent   int          // max concurrent agent executions; defaults to 1
	RateLimitPerMin int          // per-IP requests per minute; 0 disables
	InstanceID      string       // unique ID for this instance; auto-generated if empty
	FleetInstances  []string     // stable IDs shared by all nodes; enables deterministic session affinity
	DiskPaths       []string     // configured data roots sampled by the metrics endpoint
	Logger          *slog.Logger // JSON logger; defaults to stderr
	Metrics         *observability.Registry
	RequestTimeout  time.Duration // request execution deadline; defaults to 5 minutes
}

const (
	defaultRequestTimeout = 5 * time.Minute
	maxRequestBodyBytes   = 1 << 20
	idempotencyTTL        = 24 * time.Hour
	maxIdempotencyEntries = 10000
)

// Server wraps an Orchestrator in an HTTP server with REST API endpoints.
type Server struct {
	orch          *orchestrator.Orchestrator
	cfg           ServerConfig
	srv           *http.Server
	limiter       *rateLimiter
	executions    chan struct{}
	router        *SessionRouter
	metrics       *observability.Registry
	logger        *slog.Logger
	configErr     error
	draining      atomic.Bool
	idempotencyMu sync.Mutex
	idempotency   map[string]time.Time
}

// New creates a Server wired to orch. Call Start to begin serving.
func New(orch *orchestrator.Orchestrator, cfg ServerConfig) *Server {
	if cfg.Listen == "" {
		cfg.Listen = ":8430"
	}
	if cfg.MaxConcurrent <= 0 {
		// Keep the conservative deployment default; callers may opt into higher
		// concurrency now that model selection is request-scoped.
		cfg.MaxConcurrent = 1
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	if cfg.Metrics == nil {
		cfg.Metrics = observability.NewRegistry()
	}
	if cfg.AuthType == "none" {
		if strings.TrimSpace(cfg.TenantID) == "" {
			cfg.TenantID = "local"
		}
		if strings.TrimSpace(cfg.RepositoryID) == "" {
			cfg.RepositoryID = "local"
		}
	}
	if cfg.Authorizer == nil && strings.TrimSpace(cfg.RepositoryID) != "" {
		cfg.Authorizer = authorization.RepositoryAuthorizer{RepositoryID: cfg.RepositoryID, AllowedActions: serverActions()}
	}
	s := &Server{
		orch:        orch,
		cfg:         cfg,
		executions:  make(chan struct{}, cfg.MaxConcurrent),
		router:      NewSessionRouter(cfg.InstanceID, cfg.FleetInstances...),
		metrics:     cfg.Metrics,
		logger:      cfg.Logger,
		idempotency: make(map[string]time.Time),
	}
	if len(s.router.instances) > 0 && len(cfg.FleetInstances) > 0 {
		if strings.TrimSpace(cfg.InstanceID) == "" {
			s.configErr = fmt.Errorf("instance ID is required when fleet instances are configured")
		} else if !containsInstance(s.router.instances, cfg.InstanceID) {
			s.configErr = fmt.Errorf("instance ID %q is not present in fleet instances", cfg.InstanceID)
		}
	}
	if cfg.RateLimitPerMin > 0 {
		s.limiter = newRateLimiter(cfg.RateLimitPerMin, time.Minute)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /live", s.handleLive)
	mux.HandleFunc("GET /ready", s.handleReady)
	mux.HandleFunc("GET /draining", s.handleDraining)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	mux.Handle("POST /v1/chat", s.idempotentExecution(s.limitExecution(http.HandlerFunc(s.handleChat))))
	mux.Handle("POST /v1/chat/stream", s.idempotentExecution(s.limitExecution(http.HandlerFunc(s.handleChatStream))))

	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleDeleteSession)

	mux.HandleFunc("GET /v1/agents", s.handleListAgents)
	mux.HandleFunc("POST /v1/deliveries", s.handleAdmitDelivery)
	mux.HandleFunc("GET /v1/deliveries/{id}", s.handleInspectDelivery)

	mux.HandleFunc("GET /v1/memory", s.handleListMemory)
	mux.HandleFunc("POST /v1/memory", s.handleAddMemory)
	mux.HandleFunc("DELETE /v1/memory/{id}", s.handleDeleteMemory)
	mux.HandleFunc("POST /v1/memory/search", s.handleSearchMemory)

	mux.HandleFunc("GET /v1/teams", s.handleListTeams)
	mux.Handle("POST /v1/teams/{id}/run", s.idempotentExecution(s.limitExecution(http.HandlerFunc(s.handleRunTeam))))

	var handler http.Handler = mux
	switch cfg.AuthType {
	case "none":
		handler = authMiddleware("none", "", "")(handler)
	case "api_key":
		if strings.TrimSpace(cfg.APIKey) == "" {
			s.configErr = fmt.Errorf("API key is required when auth type is api_key")
			break
		}
		if strings.TrimSpace(cfg.TenantID) == "" {
			s.configErr = fmt.Errorf("tenant ID is required when auth type is api_key")
			break
		}
		if strings.TrimSpace(cfg.RepositoryID) == "" {
			s.configErr = fmt.Errorf("repository ID is required when auth type is api_key")
			break
		}
		handler = authMiddleware(cfg.AuthType, cfg.APIKey, cfg.TenantID)(tenantMiddleware(authorizationMiddleware(cfg.RepositoryID, cfg.Authorizer)(handler)))
	case "oidc":
		if strings.TrimSpace(cfg.RepositoryID) == "" {
			s.configErr = fmt.Errorf("repository ID is required when auth type is oidc")
			break
		}
		validator, err := auth.NewOIDCValidator(auth.OIDCConfig{
			Issuer:   cfg.OIDCIssuer,
			ClientID: cfg.OIDCClientID,
		})
		if err != nil {
			s.configErr = fmt.Errorf("OIDC validator: %w", err)
		} else {
			handler = oidcAuthMiddleware(validator)(tenantMiddleware(authorizationMiddleware(cfg.RepositoryID, cfg.Authorizer)(handler)))
		}
	default:
		s.configErr = fmt.Errorf("unknown auth type %q", cfg.AuthType)
	}
	if cfg.AuthType == "none" && s.configErr == nil {
		handler = localIdentityMiddleware(cfg.TenantID)(tenantMiddleware(authorizationMiddleware(cfg.RepositoryID, cfg.Authorizer)(handler)))
	}
	if s.configErr != nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"server authentication configuration is invalid"}`, http.StatusServiceUnavailable)
		})
	} else {
		if s.limiter != nil {
			handler = rateLimitMiddleware(s.limiter)(handler)
		}
		handler = corsMiddleware(cfg.CORSOrigins)(handler)
	}
	handler = requestDeadlineMiddleware(cfg.RequestTimeout)(handler)
	handler = maxBodyMiddleware(maxRequestBodyBytes)(handler)
	handler = s.requestLogMiddleware(handler)
	handler = correlationIDMiddleware(handler)
	handler = recoveryMiddleware(handler)

	s.srv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if orch != nil {
		hook := observability.NewHook(s.metrics)
		for _, id := range orch.ListAgents() {
			if agent, ok := orch.GetAgent(id); ok && agent != nil {
				agent.Hooks = append([]hooks.Hook{hook}, agent.Hooks...)
			}
		}
	}
	return s
}

func containsInstance(instances []string, instance string) bool {
	for _, candidate := range instances {
		if candidate == instance {
			return true
		}
	}
	return false
}

func (s *Server) idempotentExecution(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > 256 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key must not exceed 256 bytes"})
			return
		}
		tenant, _ := TenantIDFromContext(r.Context())
		scopedKey := tenant + "\x00" + r.URL.Path + "\x00" + key
		now := time.Now()
		s.idempotencyMu.Lock()
		for existing, created := range s.idempotency {
			if now.Sub(created) >= idempotencyTTL {
				delete(s.idempotency, existing)
			}
		}
		if _, exists := s.idempotency[scopedKey]; exists {
			s.idempotencyMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]string{"error": "idempotency key has already been used"})
			return
		}
		if len(s.idempotency) >= maxIdempotencyEntries {
			s.idempotencyMu.Unlock()
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "idempotency capacity is temporarily unavailable"})
			return
		}
		s.idempotency[scopedKey] = now
		s.idempotencyMu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) limitExecution(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "server is draining"})
			return
		}
		s.metrics.WorkQueued(1)
		select {
		case s.executions <- struct{}{}:
			s.metrics.WorkQueued(-1)
			s.metrics.WorkActive(1)
			defer func() { <-s.executions; s.metrics.WorkActive(-1) }()
			next.ServeHTTP(w, r)
		case <-r.Context().Done():
			s.metrics.WorkQueued(-1)
			writeJSON(w, http.StatusRequestTimeout, map[string]string{"error": "request canceled while waiting for execution capacity"})
		default:
			s.metrics.WorkQueued(-1)
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "maximum concurrent agent executions reached"})
		}
	})
}

// Start begins listening and serving. It blocks until the server shuts down
// or encounters a fatal error.
func (s *Server) Start() error {
	if s.configErr != nil {
		return fmt.Errorf("server: invalid configuration: %w", s.configErr)
	}
	fmt.Printf("chronos-code server listening on %s\n", s.cfg.Listen)
	err := s.srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return fmt.Errorf("server: %w", err)
}

// Shutdown gracefully shuts down the server, waiting for in-flight requests
// to complete.
func (s *Server) Shutdown(ctx context.Context) error {
	s.draining.Store(true)
	return s.srv.Shutdown(ctx)
}

func (s *Server) sampleDiskUse() {
	for _, root := range s.cfg.DiskPaths {
		var total int64
		err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if info, statErr := entry.Info(); statErr == nil {
				total += info.Size()
			}
			return nil
		})
		if err != nil {
			s.logger.Warn("disk_usage_failed", "scope", filepath.Base(root))
			continue
		}
		s.metrics.SetDiskUse(filepath.Base(root), total)
	}
}

// Handler returns the server's top-level http.Handler, useful for testing.
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}

// CleanupEphemeral removes a bounded number of expired in-memory limiter and
// idempotency entries. It is safe to call concurrently with requests.
func (s *Server) CleanupEphemeral(limit int) int {
	if limit <= 0 {
		limit = 100
	}
	now := time.Now()
	removed := 0
	if s.limiter != nil {
		removed += s.limiter.cleanup(now, limit)
	}
	remaining := limit - removed
	if remaining <= 0 {
		return removed
	}
	s.idempotencyMu.Lock()
	for key, created := range s.idempotency {
		if remaining == 0 {
			break
		}
		if now.Sub(created) >= idempotencyTTL {
			delete(s.idempotency, key)
			removed++
			remaining--
		}
	}
	s.idempotencyMu.Unlock()
	return removed
}

// ObserveCleanup publishes a retention run without exposing resource keys.
func (s *Server) ObserveCleanup(result retention.Result, err error) { s.metrics.Cleanup(result, err) }

type serverRetentionAdapter struct{ server *Server }

func (a serverRetentionAdapter) Scope() string { return retention.ScopeLimiterEntries }

func (a serverRetentionAdapter) Inventory(context.Context) ([]retention.Item, error) {
	var items []retention.Item
	if a.server.limiter != nil {
		a.server.limiter.mu.Lock()
		for key, bucket := range a.server.limiter.clients {
			items = append(items, retention.Item{Scope: retention.ScopeLimiterEntries, Key: "rate:" + key, UpdatedAt: bucket.lastReset.UTC(), Bytes: int64(len(key) + 32)})
		}
		a.server.limiter.mu.Unlock()
	}
	a.server.idempotencyMu.Lock()
	for key, created := range a.server.idempotency {
		items = append(items, retention.Item{Scope: retention.ScopeLimiterEntries, Key: "idempotency:" + key, UpdatedAt: created.UTC(), Bytes: int64(len(key) + 16)})
	}
	a.server.idempotencyMu.Unlock()
	return items, nil
}

func (a serverRetentionAdapter) Delete(_ context.Context, items []retention.Item) error {
	for _, item := range items {
		switch {
		case strings.HasPrefix(item.Key, "rate:") && a.server.limiter != nil:
			key := strings.TrimPrefix(item.Key, "rate:")
			a.server.limiter.mu.Lock()
			if bucket := a.server.limiter.clients[key]; bucket != nil && bucket.lastReset.Equal(item.UpdatedAt) {
				delete(a.server.limiter.clients, key)
			}
			a.server.limiter.mu.Unlock()
		case strings.HasPrefix(item.Key, "idempotency:"):
			key := strings.TrimPrefix(item.Key, "idempotency:")
			a.server.idempotencyMu.Lock()
			if created, ok := a.server.idempotency[key]; ok && created.Equal(item.UpdatedAt) {
				delete(a.server.idempotency, key)
			}
			a.server.idempotencyMu.Unlock()
		}
	}
	return nil
}

// RetentionAdapter exposes only the server's bounded ephemeral registries.
func (s *Server) RetentionAdapter() retention.Adapter { return serverRetentionAdapter{server: s} }
