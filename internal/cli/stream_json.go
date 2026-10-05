package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos/engine/model"
	chronosstream "github.com/spawn08/chronos/engine/stream"
)

// streamEmitter writes EventEnvelope records as JSONL: one object per line,
// a contiguous sequence from 1, a write per event, and exactly one terminal
// event that is always last. It is the only writer of stdout in stream-json
// mode.
type streamEmitter struct {
	mu     sync.Mutex
	w      io.Writer
	seq    uint64
	taskID string
	done   bool
	now    func() time.Time
}

func newStreamEmitter(w io.Writer) *streamEmitter {
	return &streamEmitter{w: w, now: time.Now}
}

// setTask sets the task id stamped on later events.
func (e *streamEmitter) setTask(id string) {
	e.mu.Lock()
	e.taskID = id
	e.mu.Unlock()
}

// emit writes one non-terminal event. Events after the terminal one are dropped.
func (e *streamEmitter) emit(kind execution.EnvelopeEventType, payload any) {
	e.write(kind, payload, false)
}

// terminate writes the terminal event once and reports whether it was written.
func (e *streamEmitter) terminate(kind execution.EnvelopeEventType, payload any) bool {
	return e.write(kind, payload, true)
}

func (e *streamEmitter) write(kind execution.EnvelopeEventType, payload any, terminal bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return false
	}
	event, err := execution.NewEvent(e.seq+1, e.now(), kind, e.taskID, payload)
	if err != nil {
		return false
	}
	line, err := json.Marshal(event)
	if err != nil {
		return false
	}
	if _, err := e.w.Write(append(line, '\n')); err != nil {
		return false
	}
	e.seq++
	if terminal {
		e.done = true
	}
	return true
}

// errorPayload is the terminal error event: the execution.Error fields plus
// the run outcome, so a consumer needs no second lookup.
type errorPayload struct {
	execution.Error
	Status     execution.Status             `json:"status"`
	StopReason execution.StopReason         `json:"stop_reason"`
	SessionID  string                       `json:"session_id"`
	Envelope   *execution.ExecutionEnvelope `json:"envelope,omitempty"`
}

// terminateWithEnvelope ends the stream from a final envelope: completion on
// success, error otherwise.
func (e *streamEmitter) terminateWithEnvelope(envelope execution.ExecutionEnvelope) {
	if envelope.Status == execution.StatusSucceeded {
		e.terminate(execution.EventCompletion, envelope)
		return
	}
	failure := execution.Error{Code: execution.ErrorInternal, Category: execution.ErrorCategoryInternal, Message: string(envelope.StopReason)}
	if envelope.Error != nil {
		failure = *envelope.Error
	}
	e.terminate(execution.EventError, errorPayload{Error: failure, Status: envelope.Status, StopReason: envelope.StopReason, SessionID: envelope.SessionID, Envelope: &envelope})
}

// terminateInvalid ends the stream for a failure before any execution began.
func (e *streamEmitter) terminateInvalid(code execution.ErrorCode, category execution.ErrorCategory, message string, status execution.Status, reason execution.StopReason) {
	e.terminate(execution.EventError, errorPayload{
		Error:  execution.Error{Code: code, Category: category, Message: message},
		Status: status, StopReason: reason,
	})
}

// streamBridge converts orchestrator activity and model stream chunks into
// events. It tracks per-model usage for the completion envelope.
type streamBridge struct {
	out      *streamEmitter
	mu       sync.Mutex
	model    func() string
	final    strings.Builder
	retries  int
	requests int
	last     model.Usage
	byModel  map[string]execution.Usage
	emitted  bool
}

// newStreamBridge reports usage under the model returned by modelID at the
// time each request completes, since routing can change it mid-run.
func newStreamBridge(out *streamEmitter, modelID func() string) *streamBridge {
	return &streamBridge{out: out, model: modelID, byModel: map[string]execution.Usage{}}
}

// finalText is the text after the last tool round: the message the model
// ended its turn with.
func (b *streamBridge) finalText() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.final.String()
}

// usageByModel returns a copy of the accumulated per-model usage.
func (b *streamBridge) usageByModel() map[string]execution.Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.byModel) == 0 {
		return nil
	}
	out := make(map[string]execution.Usage, len(b.byModel))
	for k, v := range b.byModel {
		out[k] = v
	}
	return out
}

// activity forwards broker events until ch closes or ctx ends.
func (b *streamBridge) activity(ctx context.Context, ch <-chan chronosstream.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			b.handleActivity(event)
		}
	}
}

func (b *streamBridge) handleActivity(event chronosstream.Event) {
	data, _ := event.Data.(map[string]any)
	id, _ := data["id"].(string)
	name, _ := data["tool"].(string)
	agentID, _ := data["agent"].(string)
	switch event.Type {
	case chronosstream.EventToolCall:
		payload := execution.ToolPayload{ID: id, Name: name}
		if args, ok := data["args"]; ok && args != nil {
			if raw, err := json.Marshal(args); err == nil {
				payload.Arguments = string(raw)
				if len(raw) > 0 && raw[0] == '{' {
					payload.Input = raw
				}
			}
		}
		if payload.Arguments == "" {
			payload.Arguments, payload.Input = "{}", json.RawMessage("{}")
		}
		b.out.emit(execution.EventTool, payload)
		if name == "spawn_subagent" {
			target := ""
			if args, ok := data["args"].(map[string]any); ok {
				target, _ = args["agent"].(string)
			}
			b.out.emit(execution.EventSubagent, execution.SubagentPayload{AgentID: target, Status: "started"})
		}
	case chronosstream.EventToolResult:
		payload := execution.ToolResultPayload{ID: id, Name: name, Output: data["result"]}
		if failure, ok := data["error"]; ok && failure != nil {
			payload.IsError, payload.Output = true, fmt.Sprint(failure)
		}
		b.out.emit(execution.EventToolResult, payload)
		if name == "spawn_subagent" {
			status := "completed"
			if payload.IsError {
				status = "failed"
			}
			b.out.emit(execution.EventSubagent, execution.SubagentPayload{AgentID: agentID, Status: status, Content: fmt.Sprint(payload.Output)})
		}
	case chronosstream.EventCustom:
		if kind, _ := data["type"].(string); kind == "api_retry" {
			b.mu.Lock()
			b.retries++
			attempts := b.retries
			b.mu.Unlock()
			b.out.emit(execution.EventRetry, execution.RetryPayload{Attempts: attempts, Reason: execution.StopProviderRetryable})
		}
	}
}

// chunk forwards one model stream chunk: text, reasoning and request usage.
func (b *streamBridge) chunk(resp *model.ChatResponse) {
	if resp == nil {
		return
	}
	if len(resp.ToolCalls) > 0 {
		b.mu.Lock()
		b.final.Reset()
		b.mu.Unlock()
	}
	if resp.Delta && resp.Content != "" {
		b.mu.Lock()
		b.final.WriteString(resp.Content)
		b.mu.Unlock()
	}
	if resp.Reasoning != "" {
		b.out.emit(execution.EventThinking, execution.ContentPayload{Content: resp.Reasoning, Delta: resp.Delta})
	}
	if resp.Content != "" && (resp.Delta || !b.emittedText()) {
		b.markText()
		b.out.emit(execution.EventContent, execution.ContentPayload{Content: resp.Content, Delta: resp.Delta})
	}
	if resp.Delta || (resp.Usage.PromptTokens == 0 && resp.Usage.CompletionTokens == 0 && resp.Usage.CacheReadTokens == 0 && resp.Usage.CacheCreationTokens == 0) {
		return
	}
	b.recordUsage(resp.Usage)
}

func (b *streamBridge) emittedText() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.emitted
}

func (b *streamBridge) markText() {
	b.mu.Lock()
	b.emitted = true
	b.mu.Unlock()
}

// recordUsage emits one usage event per distinct request usage.
func (b *streamBridge) recordUsage(usage model.Usage) {
	b.mu.Lock()
	if usage == b.last {
		b.mu.Unlock()
		return
	}
	b.last = usage
	b.requests++
	modelID, n := b.model(), b.requests
	entry := b.byModel[modelID]
	entry.PromptTokens += usage.PromptTokens
	entry.CompletionTokens += usage.CompletionTokens
	entry.CacheReadTokens += usage.CacheReadTokens
	entry.CacheCreationTokens += usage.CacheCreationTokens
	b.byModel[modelID] = entry
	b.mu.Unlock()
	b.out.emit(execution.EventUsage, execution.RequestUsagePayload{
		RequestID: fmt.Sprintf("req-%d", n), Model: modelID,
		Usage: execution.Usage{PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, CacheReadTokens: usage.CacheReadTokens, CacheCreationTokens: usage.CacheCreationTokens},
	})
}

// runStreamJSON executes request and writes the event stream. The returned
// error carries the process exit status; the terminal event has been written
// whenever execution began.
func runStreamJSON(ctx context.Context, orch *orchestrator.Orchestrator, request orchestrator.ExecutionRequest, out *streamEmitter, cwd string) error {
	provider, modelID := orch.EffectiveModelInfo()
	bridge := newStreamBridge(out, func() string {
		_, current := orch.EffectiveModelInfo()
		return current
	})
	activity, stop, subErr := orch.SubscribeActivity()
	defer stop()
	activityCtx, cancelActivity := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	if subErr == nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bridge.activity(activityCtx, activity)
		}()
	}
	defer func() {
		// Let queued tool events drain before the terminal event.
		time.Sleep(20 * time.Millisecond)
		cancelActivity()
		wg.Wait()
	}()
	out.emit(execution.EventSession, execution.SessionPayload{SessionID: orch.CurrentSessionID(), Model: modelID, Provider: provider, Cwd: cwd})

	var result orchestrator.ExecutionResult
	var err error
	for {
		request.Mode = orchestrator.ExecutionStreaming
		result, err = streamOnce(ctx, orch, request, bridge)
		plan, approved := orch.TakeApprovedPlan()
		if err != nil || !approved {
			break
		}
		request.Message = orchestrator.ImplementApprovedPlanMessage(plan)
	}
	envelope := orchestrator.ExecutionEnvelope(result, err, orchestrator.EnvelopeMetadata{})
	envelope.UsageByModel = bridge.usageByModel()
	if envelope.Content == "" {
		envelope.Content = bridge.finalText()
	}
	if envelope.SessionID == "" {
		envelope.SessionID = orch.CurrentSessionID()
	}
	out.setTask(envelope.TaskID)
	cancelActivity()
	wg.Wait()
	out.terminateWithEnvelope(envelope)
	if envelope.Status == execution.StatusSucceeded {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("%s", envelope.StopReason)
	}
	return &ExitError{Code: ExitCodeForStatus(envelope.Status), Err: err}
}

func streamOnce(ctx context.Context, orch *orchestrator.Orchestrator, request orchestrator.ExecutionRequest, bridge *streamBridge) (orchestrator.ExecutionResult, error) {
	result, err := orch.Execute(ctx, request)
	if err != nil {
		return result, err
	}
	var streamErr error
	for resp := range result.Stream {
		if resp == nil {
			continue
		}
		if resp.Err != nil {
			streamErr = resp.Err
			continue
		}
		bridge.chunk(resp)
	}
	completion, completed := <-result.Completion
	if completed {
		result = orchestrator.ApplyCompletion(result, completion)
	}
	if streamErr != nil {
		return result, streamErr
	}
	if completed && completion.Err != nil {
		return result, completion.Err
	}
	if cerr := ctx.Err(); cerr != nil {
		return result, cerr
	}
	return result, nil
}
