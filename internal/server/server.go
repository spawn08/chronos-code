// Package server implements PRD P3-004: an HTTP server that exposes the
// same agent capabilities as the CLI via a REST API with SSE streaming,
// authentication, rate limiting, and CORS support for team/enterprise
// deployment.
package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos-code/internal/orchestrator"
)

// ServerConfig holds configuration for the HTTP server.
type ServerConfig struct {
	Listen          string // address to listen on, e.g. ":8430"
	AuthType        string // "api_key", "oidc", or "none"
	APIKey          string // required when AuthType is "api_key"
	TenantID        string // required when AuthType is "api_key"
	OIDCIssuer      string // required when AuthType is "oidc"
	OIDCClientID    string // required when AuthType is "oidc"
	CORSOrigins     string // comma-separated allowed origins; "*" for all
	MaxConcurrent   int    // max concurrent agent executions; defaults to 1
	RateLimitPerMin int    // per-IP requests per minute; 0 disables
	InstanceID      string // unique ID for this instance; auto-generated if empty
}

// Server wraps an Orchestrator in an HTTP server with REST API endpoints.
type Server struct {
	orch       *orchestrator.Orchestrator
	cfg        ServerConfig
	srv        *http.Server
	limiter    *rateLimiter
	executions chan struct{}
	router     *SessionRouter
	configErr  error
}

// New creates a Server wired to orch. Call Start to begin serving.
func New(orch *orchestrator.Orchestrator, cfg ServerConfig) *Server {
	if cfg.Listen == "" {
		cfg.Listen = ":8430"
	}
	if cfg.MaxConcurrent <= 0 {
		// Agents currently contain request-mutated model state. Keep server
		// execution serialized by default until model selection is request-scoped.
		cfg.MaxConcurrent = 1
	}
	s := &Server{
		orch:       orch,
		cfg:        cfg,
		executions: make(chan struct{}, cfg.MaxConcurrent),
		router:     NewSessionRouter(cfg.InstanceID),
	}
	if cfg.RateLimitPerMin > 0 {
		s.limiter = newRateLimiter(cfg.RateLimitPerMin, time.Minute)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)

	mux.Handle("POST /v1/chat", s.limitExecution(http.HandlerFunc(s.handleChat)))
	mux.Handle("POST /v1/chat/stream", s.limitExecution(http.HandlerFunc(s.handleChatStream)))

	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleDeleteSession)

	mux.HandleFunc("GET /v1/agents", s.handleListAgents)

	mux.HandleFunc("GET /v1/memory", s.handleListMemory)
	mux.HandleFunc("POST /v1/memory", s.handleAddMemory)
	mux.HandleFunc("DELETE /v1/memory/{id}", s.handleDeleteMemory)
	mux.HandleFunc("POST /v1/memory/search", s.handleSearchMemory)

	mux.HandleFunc("GET /v1/teams", s.handleListTeams)
	mux.Handle("POST /v1/teams/{id}/run", s.limitExecution(http.HandlerFunc(s.handleRunTeam)))

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
		handler = authMiddleware(cfg.AuthType, cfg.APIKey, cfg.TenantID)(tenantMiddleware(handler))
	case "oidc":
		validator, err := auth.NewOIDCValidator(auth.OIDCConfig{
			Issuer:   cfg.OIDCIssuer,
			ClientID: cfg.OIDCClientID,
		})
		if err != nil {
			s.configErr = fmt.Errorf("OIDC validator: %w", err)
		} else {
			handler = oidcAuthMiddleware(validator)(tenantMiddleware(handler))
		}
	default:
		s.configErr = fmt.Errorf("unknown auth type %q", cfg.AuthType)
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

	s.srv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s
}

func (s *Server) limitExecution(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.executions <- struct{}{}:
			defer func() { <-s.executions }()
			next.ServeHTTP(w, r)
		case <-r.Context().Done():
			writeJSON(w, http.StatusRequestTimeout, map[string]string{"error": "request canceled while waiting for execution capacity"})
		default:
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
	return s.srv.Shutdown(ctx)
}

// Handler returns the server's top-level http.Handler, useful for testing.
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}
