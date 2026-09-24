package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/storage"
)

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}

func (s *Server) handleDraining(w http.ResponseWriter, _ *http.Request) {
	if s.draining.Load() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "draining"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepting"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	s.sampleDiskUse()
	if s.orch != nil {
		s.metrics.SetMCP(s.orch.MCPStatuses())
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := s.metrics.WritePrometheus(w); err != nil {
		s.logger.Error("metrics_write_failed")
	}
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	}
	if s.cfg.DeliveryWorker != nil && s.cfg.DeliveryWorker.LastError() != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "delivery worker unavailable"})
		return
	}
	if s.orch == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "orchestrator unavailable"})
		return
	}
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

// Retained for source compatibility with internal consumers of the legacy
// response shape. The HTTP handler now returns execution.ExecutionEnvelope.
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
	metadata := envelopeMetadata(r.Context())
	var req chatRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeInvalidRequest(w, jsonErrorStatus(err), "invalid JSON: "+err.Error(), metadata)
		return
	}
	if req.Message == "" {
		writeInvalidRequest(w, http.StatusBadRequest, "message is required", metadata)
		return
	}

	if req.AgentID != "" {
		if _, ok := s.orch.GetAgent(req.AgentID); !ok {
			writeInvalidRequest(w, http.StatusNotFound, fmt.Sprintf("agent %q not found", req.AgentID), metadata)
			return
		}
	}

	sid := req.SessionID
	if sid == "" {
		sid = s.newLocalSessionID()
	}
	if !s.requireLocalSession(w, sid) {
		return
	}
	started := time.Now()
	result, err := ExecuteRequest(r.Context(), s.orch, orchestrator.ExecutionRequest{
		Message:          req.Message,
		RequestedAgent:   req.AgentID,
		SessionID:        sid,
		VerificationMode: s.orch.VerificationMode(),
	})
	envelope := orchestrator.ExecutionEnvelope(result, err, metadata)
	s.observeTask(metadata, result, envelope, time.Since(started))
	if err != nil {
		writeJSON(w, envelopeHTTPStatus(envelope), envelope)
		return
	}
	if s.router != nil {
		s.router.Claim(result.SessionID)
	}

	writeJSON(w, http.StatusOK, envelope)
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	metadata := envelopeMetadata(r.Context())
	var req chatRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeInvalidRequest(w, jsonErrorStatus(err), "invalid JSON: "+err.Error(), metadata)
		return
	}
	if req.Message == "" {
		writeInvalidRequest(w, http.StatusBadRequest, "message is required", metadata)
		return
	}

	if req.AgentID != "" {
		if _, ok := s.orch.GetAgent(req.AgentID); !ok {
			writeInvalidRequest(w, http.StatusNotFound, fmt.Sprintf("agent %q not found", req.AgentID), metadata)
			return
		}
	}

	sid := req.SessionID
	if sid == "" {
		sid = s.newLocalSessionID()
	}
	if !s.requireLocalSession(w, sid) {
		return
	}

	result, err := ExecuteRequest(r.Context(), s.orch, orchestrator.ExecutionRequest{
		Message:          req.Message,
		Mode:             orchestrator.ExecutionStreaming,
		RequestedAgent:   req.AgentID,
		SessionID:        sid,
		VerificationMode: s.orch.VerificationMode(),
	})
	if err != nil {
		envelope := orchestrator.ExecutionEnvelope(result, err, metadata)
		writeJSON(w, envelopeHTTPStatus(envelope), envelope)
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

	started := time.Now()
	envelope, result := writeExecutionEventStream(r.Context(), w, flusher, result, metadata)
	s.observeTask(metadata, result, envelope, time.Since(started))
}

// ExecuteRequest runs one HTTP adapter request through the common execution
// boundary before the handler translates its result to the wire format.
func ExecuteRequest(ctx context.Context, orch *orchestrator.Orchestrator, request orchestrator.ExecutionRequest) (orchestrator.ExecutionResult, error) {
	return orch.Execute(ctx, request)
}

// WriteEventStream retains the pre-v1 stream format for internal compatibility.
// New HTTP requests use WriteExecutionEventStream.
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

// WriteExecutionEventStream writes the versioned ordered stream contract. The
// common Completion channel is authoritative for the sole terminal event.
func WriteExecutionEventStream(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, result orchestrator.ExecutionResult, metadata orchestrator.EnvelopeMetadata) {
	_, _ = writeExecutionEventStream(ctx, w, flusher, result, metadata)
}

func writeExecutionEventStream(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, result orchestrator.ExecutionResult, metadata orchestrator.EnvelopeMetadata) (execution.ExecutionEnvelope, orchestrator.ExecutionResult) {
	sequence := uint64(0)
	var content strings.Builder
	var streamErr error
	writeEvent := func(eventType execution.EnvelopeEventType, payload any) {
		sequence++
		event, err := execution.NewEvent(sequence, time.Now(), eventType, result.TaskID, payload)
		if err != nil {
			streamErr = err
			return
		}
		data, err := json.Marshal(event)
		if err != nil {
			streamErr = err
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
		flusher.Flush()
	}

	for response := range result.Stream {
		if response == nil {
			continue
		}
		if response.Err != nil {
			streamErr = response.Err
			continue
		}
		content.WriteString(response.Content)
		if response.Content != "" {
			writeEvent(execution.EventContent, execution.ContentPayload{Content: response.Content, Delta: response.Delta})
		}
		for _, call := range response.ToolCalls {
			writeEvent(execution.EventTool, execution.ToolPayload{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		}
	}

	if result.Completion != nil {
		completion, ok := <-result.Completion
		if ok {
			result = orchestrator.ApplyCompletion(result, completion)
			streamErr = completion.Err
		} else if streamErr == nil {
			streamErr = ctx.Err()
			result.StopReason = execution.StopReasonForError(streamErr)
		}
	} else if streamErr == nil {
		streamErr = ctx.Err()
		result.StopReason = execution.StopReasonForError(streamErr)
	}
	result.Response = &model.ChatResponse{Content: content.String()}
	envelope := orchestrator.ExecutionEnvelope(result, streamErr, metadata)
	if result.Budget.RepairAttempts > 0 {
		writeEvent(execution.EventRetry, execution.RetryPayload{Attempts: result.Budget.RepairAttempts})
	}
	if envelope.Usage != (execution.Usage{}) {
		writeEvent(execution.EventUsage, envelope.Usage)
	}
	writeEvent(execution.EventVerificationResult, envelope.Verification)
	if envelope.Error != nil {
		writeEvent(execution.EventError, envelope)
	} else {
		writeEvent(execution.EventCompletion, envelope)
	}
	return envelope, result
}

func (s *Server) requireLocalSession(w http.ResponseWriter, sessionID string) bool {
	if s.router == nil || !s.router.MultiInstance() || s.router.IsLocal(sessionID) {
		return true
	}
	owner := s.router.Owner(sessionID)
	w.Header().Set("X-Chronos-Session-Owner", owner)
	writeJSON(w, http.StatusConflict, map[string]string{"error": "session affinity required", "owner": owner})
	return false
}

func (s *Server) newLocalSessionID() string {
	for {
		id := session.NewSessionID()
		if s.router == nil || !s.router.MultiInstance() || s.router.IsLocal(id) {
			return id
		}
	}
}

func (s *Server) observeTask(metadata orchestrator.EnvelopeMetadata, result orchestrator.ExecutionResult, envelope execution.ExecutionEnvelope, elapsed time.Duration) {
	reason := result.StopReason
	if reason == "" {
		reason = envelope.StopReason
	}
	s.metrics.TaskOutcome(result.Budget, result.Verification, reason)
	s.logger.Info("task_terminal",
		"correlation_id", metadata.CorrelationID, "task_id", result.TaskID, "session_id", result.SessionID,
		"tenant_id", metadata.TenantID, "agent_id", result.AgentID, "status", envelope.Status,
		"stop_reason", envelope.StopReason, "duration_ms", elapsed.Milliseconds(),
		"model_calls", result.Budget.ModelCalls, "tool_calls", result.Budget.ToolCalls,
		"tokens", result.Budget.Tokens, "cost_microdollars", result.Budget.CostMicrodollars)
}

func envelopeMetadata(ctx context.Context) orchestrator.EnvelopeMetadata {
	tenantID, _ := TenantIDFromContext(ctx)
	correlationID, _ := CorrelationIDFromContext(ctx)
	return orchestrator.EnvelopeMetadata{TenantID: tenantID, CorrelationID: correlationID}
}

func writeInvalidRequest(w http.ResponseWriter, status int, message string, metadata orchestrator.EnvelopeMetadata) {
	envelope := execution.ExecutionEnvelope{
		SchemaVersion: execution.SchemaVersionV1, TenantID: metadata.TenantID, CorrelationID: metadata.CorrelationID,
		Status: execution.StatusInvalidRequest, StopReason: execution.StopInvalidRequest,
		Content: "", ChangedPaths: []string{}, Verification: execution.EnvelopeVerification{Status: execution.VerificationPending, Obligations: []execution.VerificationObligation{}},
		Error: &execution.Error{Code: execution.ErrorInvalidRequest, Category: execution.ErrorCategoryRequest, Message: message},
	}
	writeJSON(w, status, envelope)
}

func envelopeHTTPStatus(envelope execution.ExecutionEnvelope) int {
	switch envelope.Status {
	case execution.StatusSucceeded:
		return http.StatusOK
	case execution.StatusInvalidRequest:
		return http.StatusBadRequest
	case execution.StatusApprovalBlocked:
		return http.StatusConflict
	case execution.StatusRetryableProvider:
		return http.StatusServiceUnavailable
	case execution.StatusTimedOut:
		return http.StatusGatewayTimeout
	case execution.StatusBudgetExhausted:
		return http.StatusTooManyRequests
	case execution.StatusVerificationFailed:
		return http.StatusUnprocessableEntity
	case execution.StatusCancelled:
		return http.StatusRequestTimeout
	default:
		return http.StatusInternalServerError
	}
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
	if err := decodeJSONBody(r, &req); err != nil {
		writeJSON(w, jsonErrorStatus(err), map[string]string{"error": "invalid JSON: " + err.Error()})
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
	if err := decodeJSONBody(r, &req); err != nil {
		writeJSON(w, jsonErrorStatus(err), map[string]string{"error": "invalid JSON: " + err.Error()})
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
	if err := decodeJSONBody(r, &req); err != nil {
		writeJSON(w, jsonErrorStatus(err), map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}
	result, err := s.orch.RunTeam(r.Context(), id, req.Message)
	if err != nil {
		writeJSON(w, executionErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": result, "team_id": id})
}

func decodeJSONBody(r *http.Request, dst any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("request body must contain a single JSON value")
		}
		return fmt.Errorf("request body must contain a single JSON value: %w", err)
	}
	return nil
}

func executionErrorStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

func jsonErrorStatus(err error) int {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
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
