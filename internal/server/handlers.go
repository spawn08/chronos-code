package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/storage"
)

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	st := s.orch.Store()
	if st == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "no storage"})
		return
	}
	_, err := st.ListSessions(r.Context(), "", 1, 0)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "storage unreachable", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// chatRequest is the JSON body for POST /v1/chat and /v1/chat/stream.
type chatRequest struct {
	Message   string `json:"message"`
	AgentID   string `json:"agent_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// chatResponse is the JSON response for POST /v1/chat.
type chatResponse struct {
	Content   string       `json:"content"`
	Usage     usagePayload `json:"usage"`
	SessionID string       `json:"session_id"`
}

type usagePayload struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	if req.AgentID != "" {
		if _, ok := s.orch.GetAgent(req.AgentID); !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("agent %q not found", req.AgentID)})
			return
		}
	}

	sid := req.SessionID
	if sid == "" {
		sid = session.NewSessionID()
	}
	result, err := ExecuteRequest(r.Context(), s.orch, orchestrator.ExecutionRequest{
		Message:          req.Message,
		RequestedAgent:   req.AgentID,
		SessionID:        sid,
		VerificationMode: s.orch.VerificationMode(),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if s.router != nil {
		s.router.Claim(result.SessionID)
	}

	writeJSON(w, http.StatusOK, chatResponse{
		Content: result.Response.Content,
		Usage: usagePayload{
			PromptTokens:        result.Response.Usage.UncachedPromptTokens(),
			CompletionTokens:    result.Response.Usage.CompletionTokens,
			CacheReadTokens:     result.Response.Usage.CacheReadTokens,
			CacheCreationTokens: result.Response.Usage.CacheCreationTokens,
		},
		SessionID: result.SessionID,
	})
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	if req.AgentID != "" {
		if _, ok := s.orch.GetAgent(req.AgentID); !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("agent %q not found", req.AgentID)})
			return
		}
	}

	sid := req.SessionID
	if sid == "" {
		sid = session.NewSessionID()
	}

	result, err := ExecuteRequest(r.Context(), s.orch, orchestrator.ExecutionRequest{
		Message:          req.Message,
		Mode:             orchestrator.ExecutionStreaming,
		RequestedAgent:   req.AgentID,
		SessionID:        sid,
		VerificationMode: s.orch.VerificationMode(),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if s.router != nil {
		s.router.Claim(result.SessionID)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Session-ID", result.SessionID)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	WriteEventStream(r.Context(), w, flusher, result.Stream, result.SessionID)
}

// ExecuteRequest runs one HTTP adapter request through the common execution
// boundary before the handler translates its result to the wire format.
func ExecuteRequest(ctx context.Context, orch *orchestrator.Orchestrator, request orchestrator.ExecutionRequest) (orchestrator.ExecutionResult, error) {
	return orch.Execute(ctx, request)
}

// WriteEventStream translates a streaming execution result to the HTTP SSE
// contract, including terminal errors and cancellation.
func WriteEventStream(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, stream <-chan *model.ChatResponse, sessionID string) {
	for resp := range stream {
		if resp.Err != nil {
			data, _ := json.Marshal(map[string]string{"error": resp.Err.Error(), "session_id": sessionID})
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
			flusher.Flush()
			return
		}
		data, _ := json.Marshal(map[string]any{
			"content":    resp.Content,
			"delta":      resp.Delta,
			"session_id": sessionID,
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	if err := ctx.Err(); err != nil {
		data, _ := json.Marshal(map[string]string{"error": err.Error(), "session_id": sessionID})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
		flusher.Flush()
		return
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	mgr := s.orch.SessionManager()
	if mgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no session manager"})
		return
	}
	agentID := r.URL.Query().Get("agent_id")
	if agentID == "" {
		agentID = "coder"
	}
	sessions, err := mgr.List(r.Context(), agentID, 100, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	mgr := s.orch.SessionManager()
	if mgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no session manager"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	if err := mgr.Delete(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (s *Server) handleListAgents(w http.ResponseWriter, _ *http.Request) {
	ids := s.orch.ListAgents()
	agents := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		a, ok := s.orch.GetAgent(id)
		if !ok {
			continue
		}
		agents = append(agents, map[string]string{
			"id":   id,
			"name": a.Name,
		})
	}
	writeJSON(w, http.StatusOK, agents)
}

func (s *Server) handleListMemory(w http.ResponseWriter, r *http.Request) {
	ms := s.orch.MemoryStore()
	if ms == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory disabled"})
		return
	}
	ms = ms.ForContext(r.Context())
	cat := memory.Category(r.URL.Query().Get("category"))
	records, err := ms.List(cat)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, records)
}

type addMemoryRequest struct {
	Category string `json:"category"`
	Content  string `json:"content"`
}

func (s *Server) handleAddMemory(w http.ResponseWriter, r *http.Request) {
	ms := s.orch.MemoryStore()
	if ms == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory disabled"})
		return
	}
	ms = ms.ForContext(r.Context())
	var req addMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content is required"})
		return
	}
	rec, err := ms.Add(memory.Category(req.Category), req.Content)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	ms := s.orch.MemoryStore()
	if ms == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory disabled"})
		return
	}
	ms = ms.ForContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "memory id is required"})
		return
	}
	if err := ms.Forget(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

type searchMemoryRequest struct {
	Query string `json:"query"`
}

func (s *Server) handleSearchMemory(w http.ResponseWriter, r *http.Request) {
	ms := s.orch.MemoryStore()
	if ms == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory disabled"})
		return
	}
	ms = ms.ForContext(r.Context())
	var req searchMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	scored, err := ms.Recall(req.Query, 20)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, scored)
}

func (s *Server) handleListTeams(w http.ResponseWriter, _ *http.Request) {
	ids := s.orch.ListTeams()
	teams := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		t, ok := s.orch.GetTeam(id)
		if !ok {
			continue
		}
		teams = append(teams, map[string]string{
			"id":       id,
			"name":     t.Name,
			"strategy": string(t.Strategy),
		})
	}
	writeJSON(w, http.StatusOK, teams)
}

type runTeamRequest struct {
	Message string `json:"message"`
}

func (s *Server) handleRunTeam(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "team id is required"})
		return
	}
	var req runTeamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}
	result, err := s.orch.RunTeam(r.Context(), id, req.Message)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": result, "team_id": id})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// resolveCtx is an unexported helper for tests / internal use.
func resolveCtx(ctx context.Context, sessionID string) context.Context {
	if sessionID != "" {
		return storage.WithSession(ctx, sessionID)
	}
	return ctx
}
