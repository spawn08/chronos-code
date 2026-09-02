package learning

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/storage"
)

// TelemetryRecorder maps Chronos model and tool hooks to SQL telemetry.
type TelemetryRecorder struct {
	store    *SQLStore
	repoPath string
	agentID  string

	mu       sync.Mutex
	sessions map[string]struct{}
}

// NewTelemetryRecorder creates a recorder whose agentID is used when the hook
// context does not carry a session ID.
func NewTelemetryRecorder(store *SQLStore, repoPath, agentID string) *TelemetryRecorder {
	return &TelemetryRecorder{
		store:    store,
		repoPath: repoPath,
		agentID:  agentID,
		sessions: make(map[string]struct{}),
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
	sessionID := storage.SessionFromContext(ctx)
	if sessionID == "" {
		sessionID = r.agentID
	}

	content, err := eventContent(evt)
	if err != nil {
		return err
	}
	input, err := eventValue(evt.Input)
	if err != nil {
		return fmt.Errorf("learning: encode %s input: %w", evt.Type, err)
	}
	output, err := eventValue(evt.Output)
	if err != nil {
		return fmt.Errorf("learning: encode %s output: %w", evt.Type, err)
	}

	if err := r.ensureSession(ctx, sessionID, evt.Name); err != nil {
		return err
	}

	now := time.Now().UTC()
	turnID, err := telemetryID("turn")
	if err != nil {
		return err
	}
	if err := r.store.AppendTurn(ctx, Turn{
		ID:        turnID,
		SessionID: sessionID,
		Role:      string(evt.Type),
		Content:   content,
		Timestamp: now,
	}); err != nil {
		return err
	}

	if evt.Type != hooks.EventToolCallBefore && evt.Type != hooks.EventToolCallAfter {
		return nil
	}
	callID, err := telemetryID("call")
	if err != nil {
		return err
	}
	return r.store.RecordToolCall(ctx, ToolCall{
		ID:        callID,
		TurnID:    turnID,
		Name:      evt.Name,
		Input:     input,
		Output:    output,
		Timestamp: now,
	})
}

func (r *TelemetryRecorder) ensureSession(ctx context.Context, sessionID, model string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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

func eventContent(evt *hooks.Event) (string, error) {
	payload := struct {
		Name     string         `json:"name"`
		Input    any            `json:"input,omitempty"`
		Output   any            `json:"output,omitempty"`
		Metadata map[string]any `json:"metadata,omitempty"`
		Error    string         `json:"error,omitempty"`
	}{
		Name:     evt.Name,
		Input:    evt.Input,
		Output:   evt.Output,
		Metadata: evt.Metadata,
	}
	if evt.Error != nil {
		payload.Error = evt.Error.Error()
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("learning: encode %s metadata: %w", evt.Type, err)
	}
	return string(data), nil
}

func eventValue(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func telemetryID(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("learning: generate telemetry ID: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(random[:]), nil
}

var _ hooks.Hook = (*TelemetryRecorder)(nil)
