package learning

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/storage"
)

const telemetryCorrelationKey = "chronos_code_telemetry_id"

// TelemetryRecorder maps Chronos model and tool hooks to SQL telemetry.
type TelemetryRecorder struct {
	store    *SQLStore
	repoPath string
	agentID  string

	mu       sync.Mutex
	sessions map[string]struct{}
	calls    map[string]telemetryCall
}

type telemetryCall struct {
	sessionID string
	turnID    string
	toolCall  string
	kind      string
	startedAt time.Time
	completed bool
}

// NewTelemetryRecorder creates a recorder whose agentID is used when the hook
// context does not carry a session ID.
func NewTelemetryRecorder(store *SQLStore, repoPath, agentID string) *TelemetryRecorder {
	return &TelemetryRecorder{
		store:    store,
		repoPath: repoPath,
		agentID:  agentID,
		sessions: make(map[string]struct{}),
		calls:    make(map[string]telemetryCall),
	}
}

// Before records supported before-call events.
func (r *TelemetryRecorder) Before(ctx context.Context, evt *hooks.Event) error {
	if evt == nil {
		return nil
	}
	switch evt.Type {
	case hooks.EventModelCallBefore, hooks.EventToolCallBefore:
		return r.record(ctx, evt)
	default:
		return nil
	}
}

// After records supported after-call events.
func (r *TelemetryRecorder) After(ctx context.Context, evt *hooks.Event) error {
	if evt == nil {
		return nil
	}
	switch evt.Type {
	case hooks.EventModelCallAfter, hooks.EventToolCallAfter:
		return r.record(ctx, evt)
	default:
		return nil
	}
}

func (r *TelemetryRecorder) record(ctx context.Context, evt *hooks.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	sessionID := storage.SessionFromContext(ctx)
	if sessionID == "" {
		sessionID = r.agentID
	}

	callID, err := telemetryCallID(evt)
	if err != nil {
		return err
	}
	callKey := sessionID + "\x00" + callID
	call, ok := r.calls[callKey]
	if !ok {
		call, err = r.startCall(ctx, sessionID, callID, evt)
		if err != nil {
			return err
		}
		r.calls[callKey] = call
	}
	if evt.Type == hooks.EventModelCallBefore || evt.Type == hooks.EventToolCallBefore {
		return nil
	}
	if call.completed {
		return nil
	}
	return r.completeCall(ctx, callKey, callID, call, evt)
}

func (r *TelemetryRecorder) startCall(ctx context.Context, sessionID, callID string, evt *hooks.Event) (telemetryCall, error) {
	if err := r.ensureSession(ctx, sessionID, evt.Name); err != nil {
		return telemetryCall{}, err
	}
	kind := telemetryKind(evt.Type)
	now := time.Now().UTC()
	content, err := telemetryContent(evt, kind, "pending", callID)
	if err != nil {
		return telemetryCall{}, err
	}
	turnID, err := telemetryID("turn")
	if err != nil {
		return telemetryCall{}, err
	}
	if err := r.store.AppendTurn(ctx, Turn{
		ID:        turnID,
		SessionID: sessionID,
		Role:      kind,
		Content:   content,
		Timestamp: now,
	}); err != nil {
		return telemetryCall{}, err
	}
	call := telemetryCall{sessionID: sessionID, turnID: turnID, kind: kind, startedAt: now}
	if kind != "tool_call" {
		return call, nil
	}
	input, err := eventFingerprint(evt.Input)
	if err != nil {
		return telemetryCall{}, fmt.Errorf("learning: fingerprint tool input: %w", err)
	}
	toolCallID, err := telemetryID("call")
	if err != nil {
		return telemetryCall{}, err
	}
	if err := r.store.RecordToolCall(ctx, ToolCall{
		ID:        toolCallID,
		TurnID:    turnID,
		Name:      evt.Name,
		Input:     input,
		Timestamp: now,
	}); err != nil {
		return telemetryCall{}, err
	}
	call.toolCall = toolCallID
	return call, nil
}

func (r *TelemetryRecorder) completeCall(ctx context.Context, callKey, callID string, call telemetryCall, evt *hooks.Event) error {
	now := time.Now().UTC()
	content, err := telemetryContent(evt, call.kind, "completed", callID)
	if err != nil {
		return err
	}
	if _, err := r.store.db.ExecContext(ctx, `UPDATE turns SET content = ?, ts = ? WHERE id = ?`, content, timestamp(now), call.turnID); err != nil {
		return fmt.Errorf("learning: complete telemetry turn %q: %w", call.turnID, err)
	}
	if call.toolCall != "" {
		output, err := eventFingerprint(evt.Output)
		if err != nil {
			return fmt.Errorf("learning: fingerprint tool output: %w", err)
		}
		if _, err := r.store.db.ExecContext(ctx, `UPDATE tool_calls SET output = ?, duration_ms = ?, ts = ? WHERE id = ?`, output, now.Sub(call.startedAt).Milliseconds(), timestamp(now), call.toolCall); err != nil {
			return fmt.Errorf("learning: complete telemetry tool call %q: %w", call.toolCall, err)
		}
	}
	inputTokens, outputTokens := usage(evt)
	if _, err := r.store.db.ExecContext(ctx, `
		UPDATE sessions
		SET turns = turns + 1, input_tokens = input_tokens + ?, output_tokens = output_tokens + ?
		WHERE id = ?`, inputTokens, outputTokens, call.sessionID); err != nil {
		return fmt.Errorf("learning: update telemetry totals for session %q: %w", call.sessionID, err)
	}
	call.completed = true
	r.calls[callKey] = call
	return nil
}

func (r *TelemetryRecorder) ensureSession(ctx context.Context, sessionID, model string) error {
	if _, ok := r.sessions[sessionID]; ok {
		return nil
	}
	if err := r.store.CreateSession(ctx, Session{
		ID:        sessionID,
		RepoPath:  r.repoPath,
		StartedAt: time.Now().UTC(),
		Model:     model,
	}); err != nil {
		return err
	}
	r.sessions[sessionID] = struct{}{}
	return nil
}

func telemetryCallID(evt *hooks.Event) (string, error) {
	if evt.Metadata == nil {
		evt.Metadata = make(map[string]any)
	}
	if id, ok := evt.Metadata[telemetryCorrelationKey].(string); ok && id != "" {
		return id, nil
	}
	id, err := telemetryID("operation")
	if err != nil {
		return "", err
	}
	evt.Metadata[telemetryCorrelationKey] = id
	return id, nil
}

func telemetryKind(eventType hooks.EventType) string {
	if eventType == hooks.EventToolCallBefore || eventType == hooks.EventToolCallAfter {
		return "tool_call"
	}
	return "model_call"
}

func telemetryContent(evt *hooks.Event, kind, state, callID string) (string, error) {
	payload := struct {
		Name          string `json:"name"`
		Kind          string `json:"kind"`
		State         string `json:"state"`
		CorrelationID string `json:"correlation_id"`
		InputHash     string `json:"input_hash,omitempty"`
		OutputHash    string `json:"output_hash,omitempty"`
		Failed        bool   `json:"failed,omitempty"`
	}{
		Name:          evt.Name,
		Kind:          kind,
		State:         state,
		CorrelationID: callID,
	}
	var err error
	if payload.InputHash, err = eventFingerprint(evt.Input); err != nil {
		return "", fmt.Errorf("learning: fingerprint %s input: %w", evt.Type, err)
	}
	if state == "completed" {
		if payload.OutputHash, err = eventFingerprint(evt.Output); err != nil {
			return "", fmt.Errorf("learning: fingerprint %s output: %w", evt.Type, err)
		}
		payload.Failed = evt.Error != nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("learning: encode %s telemetry metadata: %w", evt.Type, err)
	}
	return string(data), nil
}

func eventFingerprint(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func usage(evt *hooks.Event) (int, int) {
	if evt.Error != nil {
		return 0, 0
	}
	response, ok := evt.Output.(*model.ChatResponse)
	if !ok || response == nil {
		return 0, 0
	}
	return response.Usage.PromptTokens, response.Usage.CompletionTokens
}

func telemetryID(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("learning: generate telemetry ID: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(random[:]), nil
}

var _ hooks.Hook = (*TelemetryRecorder)(nil)
