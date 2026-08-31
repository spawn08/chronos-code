package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/session"
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
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
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

	agentID := req.AgentID
	if agentID == "" {
		agentID = s.orch.ActiveID()
	}
	a, ok := s.orch.GetAgent(agentID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("agent %q not found", agentID)})
		return
	}

	sid := req.SessionID
	if sid == "" {
		sid = session.NewSessionID()
	}
	mgr := s.orch.SessionManager()
	if mgr != nil {
		if err := mgr.Ensure(r.Context(), sid, agentID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ensure session: " + err.Error()})
			return
		}
	}

	resp, err := a.ChatWithSession(r.Context(), sid, req.Message)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, chatResponse{
		Content: resp.Content,
		Usage: usagePayload{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
		},
		SessionID: sid,
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

	agentID := req.AgentID
	if agentID == "" {
		agentID = s.orch.ActiveID()
	}
	a, ok := s.orch.GetAgent(agentID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("agent %q not found", agentID)})
		return
	}

	sid := req.SessionID
	if sid == "" {
		sid = session.NewSessionID()
	}

	ctx := r.Context()
	if sid != "" {
		ctx = storage.WithSession(ctx, sid)
	}

	ch, err := a.ChatStream(ctx, req.Message)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Session-ID", sid)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	for resp := range ch {
		data, _ := json.Marshal(map[string]any{
			"content":    resp.Content,
			"delta":      resp.Delta,
			"session_id": sid,
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
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
