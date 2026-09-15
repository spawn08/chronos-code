package learning

import (
	"container/list"
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

// TelemetryRecorderOptions configures the opt-in asynchronous recorder. Values
// <= 0 use the defaults shown below. Limits count events/calls, not bytes.
type TelemetryRecorderOptions struct {
	QueueCapacity      int           // Default: 256 queued events, plus one in flight.
	CompletedCallLimit int           // Default: 1024 recently completed calls.
	PendingCallLimit   int           // Default: 1024 unfinished calls.
	WriteTimeout       time.Duration // Default: 5s per dequeued event.
}

// TelemetryRecorderStats is a cumulative, concurrency-safe snapshot.
type TelemetryRecorderStats struct {
	Accepted  uint64 // Events accepted for recording (including duplicates).
	Processed uint64 // Accepted events attempted, whether successful or failed.
	Dropped   uint64 // Events rejected because the queue is full or recorder closed.
	Errors    uint64 // Snapshot or persistence failures; drops are not errors.
}

// TelemetryRecorder maps Chronos model and tool hooks to SQL telemetry.
type TelemetryRecorder struct {
	store    *SQLStore
	repoPath string
	agentID  string

	// mu serializes ingestion/lifecycle, never asynchronous SQL. Correlation
	// state is owned by the worker in async mode and by mu in sync mode.
	mu       sync.Mutex
	options  TelemetryRecorderOptions
	queue    chan telemetryEvent
	done     chan struct{}
	progress chan struct{}
	closed   bool
	stats    TelemetryRecorderStats
	firstErr error
	calls    map[string]telemetryCall
	pending  list.List
	complete list.List
}

// telemetryEvent contains only immutable, privacy-safe values. In particular,
// it retains no context, metadata map, input/output object, or event error.
type telemetryEvent struct {
	typ          hooks.EventType
	name         string
	sessionID    string
	callID       string
	inputHash    string
	outputHash   string
	failed       bool
	inputTokens  int
	outputTokens int
	at           time.Time
}

type telemetryCall struct {
	sessionID string
	turnID    string
	toolCall  string
	kind      string
	startedAt time.Time
	completed bool
	order     *list.Element
}

// NewTelemetryRecorder creates a recorder whose agentID is used when the hook
// context does not carry a session ID. Hooks persist synchronously and return
// storage errors, as before. Correlation retention uses the default limits.
func NewTelemetryRecorder(store *SQLStore, repoPath, agentID string) *TelemetryRecorder {
	return &TelemetryRecorder{
		store:    store,
		repoPath: repoPath,
		agentID:  agentID,
		options:  telemetryOptions(TelemetryRecorderOptions{}),
		done:     make(chan struct{}),
		progress: make(chan struct{}),
		calls:    make(map[string]telemetryCall),
	}
}

// NewAsyncTelemetryRecorder starts one bounded worker. Hooks snapshot/fingerprint
// caller-owned data before returning and never return telemetry errors. Callers
// must not mutate an event concurrently with a hook, but may reuse it as soon as
// the hook returns. Call Close before closing the borrowed SQLStore.
func NewAsyncTelemetryRecorder(store *SQLStore, repoPath, agentID string, options TelemetryRecorderOptions) *TelemetryRecorder {
	r := NewTelemetryRecorder(store, repoPath, agentID)
	r.options = telemetryOptions(options)
	r.queue = make(chan telemetryEvent, r.options.QueueCapacity)
	go r.run()
	return r
}

func telemetryOptions(options TelemetryRecorderOptions) TelemetryRecorderOptions {
	if options.QueueCapacity <= 0 {
		options.QueueCapacity = 256
	}
	if options.CompletedCallLimit <= 0 {
		options.CompletedCallLimit = 1024
	}
	if options.PendingCallLimit <= 0 {
		options.PendingCallLimit = 1024
	}
	if options.WriteTimeout <= 0 {
		options.WriteTimeout = 5 * time.Second
	}
	return options
}

// Stats returns counters without waiting for asynchronous database writes.
func (r *TelemetryRecorder) Stats() TelemetryRecorderStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

// Flush waits for all events accepted before this call to be attempted, then
// returns the first recorded error (sticky for the recorder's lifetime). Drops
// are reported only by Stats. Concurrent producers may continue submitting work.
// Context cancellation stops waiting, not the worker or accepted writes.
func (r *TelemetryRecorder) Flush(ctx context.Context) error {
	r.mu.Lock()
	target := r.stats.Accepted
	for r.stats.Processed < target {
		progress := r.progress
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-progress:
		}
		r.mu.Lock()
	}
	err := r.firstErr
	r.mu.Unlock()
	return err
}

// Close stops accepting events, drains the queue, and joins the worker. It is
// safe to call concurrently or repeatedly. A context timeout only stops waiting;
// call Close again to join before closing the store. Each async write has its own
// WriteTimeout. Close returns the sticky first error and never closes SQLStore.
func (r *TelemetryRecorder) Close(ctx context.Context) error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		if r.queue != nil {
			close(r.queue)
		} else {
			r.clearCalls()
			close(r.done)
		}
	}
	r.mu.Unlock()
	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.firstErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *TelemetryRecorder) run() {
	defer close(r.done)
	defer r.clearCalls()
	for evt := range r.queue {
		ctx, cancel := context.WithTimeout(context.Background(), r.options.WriteTimeout)
		err := r.record(ctx, evt)
		cancel()
		r.mu.Lock()
		r.recordError(err)
		r.stats.Processed++
		close(r.progress)
		r.progress = make(chan struct{})
		r.mu.Unlock()
	}
}

func (r *TelemetryRecorder) clearCalls() {
	clear(r.calls)
	r.pending.Init()
	r.complete.Init()
}

// recordError is called with mu held and retains only the first error.
func (r *TelemetryRecorder) recordError(err error) {
	if err != nil {
		r.stats.Errors++
		if r.firstErr == nil {
			r.firstErr = err
		}
	}
}

// Before records supported before-call events.
func (r *TelemetryRecorder) Before(ctx context.Context, evt *hooks.Event) error {
	if evt == nil {
		return nil
	}
	switch evt.Type {
	case hooks.EventModelCallBefore, hooks.EventToolCallBefore:
		return r.submit(ctx, evt)
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
		return r.submit(ctx, evt)
	default:
		return nil
	}
}

func (r *TelemetryRecorder) submit(ctx context.Context, evt *hooks.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		r.stats.Dropped++
		if r.queue != nil {
			return nil
		}
		return fmt.Errorf("learning: telemetry recorder closed")
	}
	snapshot, err := r.snapshot(ctx, evt)
	if err != nil {
		if r.queue != nil {
			// A custom JSON marshaler's error can retain caller-owned objects or
			// secret values. Keep a fixed diagnostic for asynchronous snapshots.
			r.recordError(fmt.Errorf("learning: snapshot telemetry event failed"))
			return nil
		}
		r.recordError(err)
		return err
	}
	if r.queue != nil {
		select {
		case r.queue <- snapshot:
			r.stats.Accepted++
		default:
			r.stats.Dropped++
		}
		return nil
	}
	r.stats.Accepted++
	err = r.record(ctx, snapshot)
	r.stats.Processed++
	r.recordError(err)
	return err
}

func (r *TelemetryRecorder) snapshot(ctx context.Context, evt *hooks.Event) (telemetryEvent, error) {
	sessionID := storage.SessionFromContext(ctx)
	if sessionID == "" {
		sessionID = r.agentID
	}

	callID, err := telemetryCallID(evt)
	if err != nil {
		return telemetryEvent{}, err
	}
	snapshot := telemetryEvent{
		typ: evt.Type, name: evt.Name, sessionID: sessionID, callID: callID,
		failed: evt.Error != nil, at: time.Now().UTC(),
	}
	if snapshot.inputHash, err = eventFingerprint(evt.Input); err != nil {
		return telemetryEvent{}, fmt.Errorf("learning: fingerprint %s input: %w", evt.Type, err)
	}
	if evt.Type == hooks.EventModelCallAfter || evt.Type == hooks.EventToolCallAfter {
		if snapshot.outputHash, err = eventFingerprint(evt.Output); err != nil {
			return telemetryEvent{}, fmt.Errorf("learning: fingerprint %s output: %w", evt.Type, err)
		}
		snapshot.inputTokens, snapshot.outputTokens = usage(evt)
	}
	return snapshot, nil
}

func (r *TelemetryRecorder) record(ctx context.Context, evt telemetryEvent) error {
	callKey := evt.sessionID + "\x00" + evt.callID
	call, ok := r.calls[callKey]
	if !ok {
		var err error
		call, err = r.startCall(ctx, evt)
		if err != nil {
			return err
		}
		call.order = r.pending.PushBack(callKey)
		r.calls[callKey] = call
		r.trimCalls(&r.pending, r.options.PendingCallLimit)
	}
	if evt.typ == hooks.EventModelCallBefore || evt.typ == hooks.EventToolCallBefore {
		return nil
	}
	if call.completed {
		return nil
	}
	return r.completeCall(ctx, callKey, call, evt)
}

// Each FIFO and its map entries have a fixed bound. Eviction ends correlation /
// duplicate suppression for that call; persisted pending rows remain available.
func (r *TelemetryRecorder) trimCalls(order *list.List, limit int) {
	for order.Len() > limit {
		oldest := order.Front()
		delete(r.calls, oldest.Value.(string))
		order.Remove(oldest)
	}
}

func (r *TelemetryRecorder) startCall(ctx context.Context, evt telemetryEvent) (telemetryCall, error) {
	if err := r.ensureSession(ctx, evt.sessionID, evt.name); err != nil {
		return telemetryCall{}, err
	}
	kind := telemetryKind(evt.typ)
	now := evt.at
	content, err := telemetryContent(evt, kind, "pending")
	if err != nil {
		return telemetryCall{}, err
	}
	turnID, err := telemetryID("turn")
	if err != nil {
		return telemetryCall{}, err
	}
	if err := r.store.AppendTurn(ctx, Turn{
		ID:        turnID,
		SessionID: evt.sessionID,
		Role:      kind,
		Content:   content,
		Timestamp: now,
	}); err != nil {
		return telemetryCall{}, err
	}
	call := telemetryCall{sessionID: evt.sessionID, turnID: turnID, kind: kind, startedAt: now}
	if kind != "tool_call" {
		return call, nil
	}
	toolCallID, err := telemetryID("call")
	if err != nil {
		return telemetryCall{}, err
	}
	if err := r.store.RecordToolCall(ctx, ToolCall{
		ID:        toolCallID,
		TurnID:    turnID,
		Name:      evt.name,
		Input:     evt.inputHash,
		Timestamp: now,
	}); err != nil {
		return telemetryCall{}, err
	}
	call.toolCall = toolCallID
	return call, nil
}

func (r *TelemetryRecorder) completeCall(ctx context.Context, callKey string, call telemetryCall, evt telemetryEvent) error {
	now := evt.at
	content, err := telemetryContent(evt, call.kind, "completed")
	if err != nil {
		return err
	}
	if _, err := r.store.db.ExecContext(ctx, `UPDATE turns SET content = ?, ts = ? WHERE id = ?`, content, timestamp(now), call.turnID); err != nil {
		return fmt.Errorf("learning: complete telemetry turn %q: %w", call.turnID, err)
	}
	if call.toolCall != "" {
		if _, err := r.store.db.ExecContext(ctx, `UPDATE tool_calls SET output = ?, duration_ms = ?, ts = ? WHERE id = ?`, evt.outputHash, now.Sub(call.startedAt).Milliseconds(), timestamp(now), call.toolCall); err != nil {
			return fmt.Errorf("learning: complete telemetry tool call %q: %w", call.toolCall, err)
		}
	}
	if _, err := r.store.db.ExecContext(ctx, `
		UPDATE sessions
		SET turns = turns + 1, input_tokens = input_tokens + ?, output_tokens = output_tokens + ?
		WHERE id = ?`, evt.inputTokens, evt.outputTokens, call.sessionID); err != nil {
		return fmt.Errorf("learning: update telemetry totals for session %q: %w", call.sessionID, err)
	}
	call.completed = true
	r.pending.Remove(call.order)
	call.order = r.complete.PushBack(callKey)
	r.calls[callKey] = call
	r.trimCalls(&r.complete, r.options.CompletedCallLimit)
	return nil
}

func (r *TelemetryRecorder) ensureSession(ctx context.Context, sessionID, model string) error {
	// CreateSession is an idempotent insert. Avoid an unbounded session cache.
	return r.store.CreateSession(ctx, Session{
		ID:        sessionID,
		RepoPath:  r.repoPath,
		StartedAt: time.Now().UTC(),
		Model:     model,
	})
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

func telemetryContent(evt telemetryEvent, kind, state string) (string, error) {
	payload := struct {
		Name          string `json:"name"`
		Kind          string `json:"kind"`
		State         string `json:"state"`
		CorrelationID string `json:"correlation_id"`
		InputHash     string `json:"input_hash,omitempty"`
		OutputHash    string `json:"output_hash,omitempty"`
		Failed        bool   `json:"failed,omitempty"`
	}{
		Name:          evt.name,
		Kind:          kind,
		State:         state,
		CorrelationID: evt.callID,
		InputHash:     evt.inputHash,
	}
	if state == "completed" {
		payload.OutputHash = evt.outputHash
		payload.Failed = evt.failed
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("learning: encode %s telemetry metadata: %w", evt.typ, err)
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
