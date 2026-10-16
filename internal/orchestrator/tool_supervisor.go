package orchestrator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

type supervisedTool struct {
	mu          sync.Mutex
	receiptMu   sync.Mutex
	id          string
	name        string
	owner       string
	callID      string
	invocation  string
	arguments   json.RawMessage
	fingerprint string
	started     time.Time
	observed    time.Time
	done        chan struct{}
	cancel      context.CancelFunc
	result      any
	err         error
	receiptErr  error
	finished    bool
	cancelAsked bool
	inbox       *SteeringInbox
	undelivered []string
}

type supervisedInboxKey struct{}

type toolSupervisor struct {
	mu     sync.Mutex
	runs   map[string]*supervisedTool
	active map[string]string
	gate   chan struct{}
	yield  time.Duration
	store  storage.Storage
	closed bool
}

func toolOwner(ctx context.Context) string {
	return storage.TenantFromContext(ctx) + "\x00" + storage.SessionFromContext(ctx)
}

func (s *toolSupervisor) persist(ctx context.Context, run *supervisedTool) error {
	// Serialize observations so cancellation cannot overwrite a terminal receipt.
	run.receiptMu.Lock()
	defer run.receiptMu.Unlock()
	return s.writeReceipt(ctx, run)
}

func (s *toolSupervisor) writeReceipt(ctx context.Context, run *supervisedTool) error {
	files, ok := s.store.(storage.SessionFileStore)
	if !ok || storage.SessionFromContext(ctx) == "" {
		return fmt.Errorf("tool supervision requires session file storage and a session")
	}
	receipt := s.report(run)
	receipt["arguments"], receipt["invocation_id"] = run.arguments, run.invocation
	data, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode tool receipt: %w", err)
	}
	durable, cancel := durableObservationContext(ctx)
	defer cancel()
	if err := files.PutFile(durable, storage.SessionFromContext(ctx), "harness/runs/"+run.id, data); err != nil {
		return err
	}
	key := fmt.Sprintf("harness/active/%x", sha256.Sum256([]byte(run.fingerprint)))
	return files.PutFile(durable, storage.SessionFromContext(ctx), key, data)
}

func errorText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

func (s *toolSupervisor) start(ctx context.Context, name string, args map[string]any, original tool.Handler) (any, error) {
	if _, durable := execution.OperationLeaseFromContext(ctx); durable {
		return original(ctx, args)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := s.store.(storage.SessionFileStore); !ok || storage.SessionFromContext(ctx) == "" {
		return nil, fmt.Errorf("tool supervision requires session file storage and a session")
	}
	data, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	// Detach arguments and request cancellation. Only cancel_tool/Close cancel
	// background work; an await call bounds its own wait.
	var copied map[string]any
	if err := json.Unmarshal(data, &copied); err != nil {
		return nil, err
	}
	callID, _ := agent.ToolCallIDFromContext(ctx)
	identity, _ := agent.RunIdentityFromContext(ctx)
	fingerprint := toolOwner(ctx) + "\x00" + name + "\x00" + canonicalArgs(string(data))
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("tool supervisor closed")
	}
	if id := s.active[fingerprint]; id != "" {
		run := s.runs[id]
		s.mu.Unlock()
		return s.report(run), nil
	}
	files := s.store.(storage.SessionFileStore)
	key := fmt.Sprintf("harness/active/%x", sha256.Sum256([]byte(fingerprint)))
	lookup, cancelLookup := context.WithTimeout(ctx, 5*time.Second)
	priorData, priorErr := files.GetFile(lookup, storage.SessionFromContext(ctx), key)
	cancelLookup()
	if priorErr == nil {
		var prior map[string]any
		if err := json.Unmarshal(priorData, &prior); err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("decode prior tool receipt: %w", err)
		}
		if prior["status"] != "completed" || callID != "" && prior["call_id"] == callID {
			s.mu.Unlock()
			if prior["status"] != "completed" {
				prior["status"] = "interrupted"
				prior["hint"] = "Original effect outcome is unknown. Reconcile its destination; do not relaunch it."
			}
			return prior, nil
		}
	} else if !errors.Is(priorErr, storage.ErrFileNotFound) {
		s.mu.Unlock()
		return nil, fmt.Errorf("read prior tool receipt: %w", priorErr)
	}
	select {
	case s.gate <- struct{}{}:
	default:
		s.mu.Unlock()
		return nil, fmt.Errorf("background tool capacity reached; await or cancel an existing tool; cancellation does not release capacity until its handler exits")
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	now := time.Now()
	run := &supervisedTool{id: "run-" + rand.Text(), name: name, owner: toolOwner(ctx), callID: callID, invocation: identity.InvocationID, arguments: data, fingerprint: fingerprint, started: now, observed: now, done: make(chan struct{}), cancel: cancel}
	if name == "spawn_subagent" {
		run.inbox = NewSteeringInbox()
		runCtx = context.WithValue(runCtx, supervisedInboxKey{}, run.inbox)
	}
	s.runs[run.id], s.active[fingerprint] = run, run.id
	// Keep admission serialized until the intent is durable: a concurrent
	// duplicate must never receive a run that has not actually been launched.
	if err := s.persist(ctx, run); err != nil {
		delete(s.runs, run.id)
		delete(s.active, fingerprint)
		s.mu.Unlock()
		<-s.gate
		cancel()
		if run.inbox != nil {
			run.inbox.Close()
		}
		return nil, fmt.Errorf("journal background tool: %w", err)
	}
	s.mu.Unlock()
	go func() {
		result, callErr := invokeSupervised(runCtx, copied, original)
		var undelivered []string
		if run.inbox != nil {
			undelivered = run.inbox.Close()
		}
		run.mu.Lock()
		run.result, run.err, run.finished, run.observed, run.undelivered = result, callErr, true, time.Now(), undelivered
		run.mu.Unlock()
		persistErr := s.persist(ctx, run)
		run.mu.Lock()
		run.receiptErr = persistErr
		run.mu.Unlock()
		cancel()
		// Uncommitted receipts retain admission as well as their result so
		// repeated storage failures cannot grow unresolved work without bound.
		if persistErr == nil {
			<-s.gate
		}
		s.mu.Lock()
		if persistErr == nil {
			delete(s.active, fingerprint)
			delete(s.runs, run.id)
		}
		s.mu.Unlock()
		close(run.done)
	}()
	yield := s.yield
	if name == "spawn_subagent" {
		yield = 0
	}
	timer := time.NewTimer(yield)
	defer timer.Stop()
	select {
	case <-run.done:
		run.mu.Lock()
		defer run.mu.Unlock()
		if run.receiptErr != nil {
			return supervisedReceipt(run), nil
		}
		return run.result, run.err
	case <-timer.C:
		return s.report(run), nil
	case <-ctx.Done():
		return s.report(run), nil
	}
}

func invokeSupervised(ctx context.Context, args map[string]any, original tool.Handler) (result any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tool handler panicked: %v; effects may require reconciliation", recovered)
		}
	}()
	return original(ctx, args)
}

func (s *toolSupervisor) report(run *supervisedTool) map[string]any {
	run.mu.Lock()
	defer run.mu.Unlock()
	return supervisedReceipt(run)
}

func supervisedReceipt(run *supervisedTool) map[string]any {
	result := map[string]any{"tool_run_id": run.id, "tool": run.name, "call_id": run.callID, "started_at": run.started, "last_observed_at": run.observed, "heartbeat_at": time.Now(), "elapsed_ms": time.Since(run.started).Milliseconds(), "idle_ms": time.Since(run.observed).Milliseconds(), "cancel_requested": run.cancelAsked}
	if run.finished {
		result["status"], result["result"] = "completed", run.result
		if run.err != nil {
			result["error"] = run.err.Error()
		}
		if len(run.undelivered) > 0 {
			result["undelivered_messages"] = run.undelivered
		}
		if run.receiptErr != nil {
			result["status"], result["receipt_error"] = "reconciliation_required", run.receiptErr.Error()
			result["hint"] = "The handler finished but its result receipt was not durably committed. Preserve this result and use await_tool to retry committing its receipt; reconcile the original effect, never repeat it."
		}
	} else {
		result["status"] = "running"
		if run.cancelAsked {
			result["status"] = "cancel_requested"
		}
		result["hint"] = "Use await_tool to collect the original result. Continue independent work; do not repeat this tool. Cancellation requests do not undo effects or free capacity before the handler exits. Heartbeat reports host observation, not handler progress. Idle time is time since the last lifecycle observation, not proof that work stopped."
	}
	return result
}

func (s *toolSupervisor) await(ctx context.Context, id string, wait time.Duration, cancelRun bool) (any, error) {
	s.mu.Lock()
	run := s.runs[id]
	s.mu.Unlock()
	if run == nil || run.owner != toolOwner(ctx) {
		files, ok := s.store.(storage.SessionFileStore)
		if !ok || storage.SessionFromContext(ctx) == "" {
			return nil, fmt.Errorf("tool run unavailable")
		}
		data, err := files.GetFile(ctx, storage.SessionFromContext(ctx), "harness/runs/"+id)
		if err != nil {
			return nil, fmt.Errorf("tool run unavailable in this session")
		}
		var result map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, err
		}
		if result["status"] != "completed" {
			result["status"] = "interrupted"
			result["hint"] = "No terminal receipt is available on this host. The effect outcome is unknown; reconcile the destination before repeating it."
		}
		return result, nil
	}
	if cancelRun {
		run.cancel()
		run.mu.Lock()
		run.cancelAsked, run.observed = true, time.Now()
		run.mu.Unlock()
		if err := s.persist(ctx, run); err != nil {
			return s.report(run), fmt.Errorf("persist cancellation request: %w", err)
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-run.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	select {
	case <-run.done:
		// Preserve the original result even when receipt storage is unavailable.
		_ = s.repairReceipt(ctx, run)
	default:
	}
	return s.report(run), nil
}

func (s *toolSupervisor) repairReceipt(ctx context.Context, run *supervisedTool) error {
	run.receiptMu.Lock()
	defer run.receiptMu.Unlock()
	run.mu.Lock()
	if run.receiptErr == nil {
		run.mu.Unlock()
		return nil
	}
	run.receiptErr = nil
	run.mu.Unlock()
	if err := s.writeReceipt(ctx, run); err != nil {
		run.mu.Lock()
		run.receiptErr = err
		run.mu.Unlock()
		return err
	}
	s.mu.Lock()
	if s.runs[run.id] == run {
		delete(s.runs, run.id)
		delete(s.active, run.fingerprint)
		<-s.gate
	}
	s.mu.Unlock()
	return nil
}

func (s *toolSupervisor) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, run := range s.runs {
		run.cancel()
		run.mu.Lock()
		run.cancelAsked, run.observed = true, time.Now()
		run.mu.Unlock()
	}
}

func (s *toolSupervisor) Running(ctx context.Context) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lines []string
	for _, run := range s.runs {
		if run.owner == toolOwner(ctx) {
			report := s.report(run)
			lines = append(lines, fmt.Sprintf("%s %s %s; idle_ms=%v; await_tool collects its original result", run.id, run.name, report["status"], report["idle_ms"]))
		}
	}
	sort.Strings(lines)
	return lines
}

func (o *Orchestrator) installToolSupervision(a *agent.Agent) {
	s := o.toolSupervisor
	if s == nil {
		return
	}
	for _, def := range a.Tools.List() {
		if def.Handler == nil || def.Name == "shell" || def.Name == "shell_auto" || def.Name == "file_write" || def.Name == "await_shell" || def.Name == "kill_shell" || def.Name == "call_tool" {
			continue
		}
		// Pure local readers remain synchronous and avoid a persistence round
		// trip; foreign handlers and delegation get non-blocking supervision.
		if !supervisedName(def.Name) {
			continue
		}
		wrapped := *def
		original := def.Handler
		wrapped.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			return s.start(ctx, wrapped.Name, args, original)
		}
		a.Tools.Register(&wrapped)
	}
	for _, name := range []string{"await_tool", "cancel_tool"} {
		name := name
		a.Tools.Register(&tool.Definition{Name: name, Permission: tool.PermAllow, ParallelSafe: true, Effects: []tool.Effect{tool.EffectRead}, Description: "Collect or request cancellation of an already-authorized background tool/subagent. timeout_ms bounds this wait only. Completed receipts survive host restart; interrupted effects must be reconciled, never blindly repeated.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"tool_run_id": map[string]any{"type": "string", "pattern": "^run-[A-Za-z0-9]+$"}, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 120000}}, "required": []string{"tool_run_id"}}, Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, _ := args["tool_run_id"].(string)
			if _, err := artifactPath(id); err != nil {
				return nil, err
			}
			ms := 30000
			if name == "cancel_tool" {
				ms = 0
			}
			if n, ok := intValue(args["timeout_ms"]); ok {
				ms = n
			}
			if ms < 0 || ms > 120000 {
				return nil, fmt.Errorf("invalid timeout_ms")
			}
			return s.await(ctx, id, time.Duration(ms)*time.Millisecond, name == "cancel_tool")
		}})
	}
	a.Tools.Register(&tool.Definition{Name: "message_tool", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectRead}, Description: "Send a correction or additional context to a running background subagent at its next tool boundary.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"tool_run_id": map[string]any{"type": "string"}, "message": map[string]any{"type": "string", "maxLength": 4096}}, "required": []string{"tool_run_id", "message"}}, Handler: func(ctx context.Context, args map[string]any) (any, error) {
		id, _ := args["tool_run_id"].(string)
		text, _ := args["message"].(string)
		if text == "" || len(text) > 4096 {
			return nil, fmt.Errorf("message must contain 1–4096 bytes")
		}
		s.mu.Lock()
		run := s.runs[id]
		s.mu.Unlock()
		if run == nil || run.owner != toolOwner(ctx) || run.inbox == nil || !run.inbox.Push(text) {
			return nil, fmt.Errorf("running subagent unavailable in this session")
		}
		return map[string]any{"tool_run_id": id, "status": "message_queued"}, nil
	}})
}

func supervisedName(name string) bool {
	switch name {
	case "file_read", "file_write", "file_edit", "file_list", "file_glob", "file_grep", "shell", "shell_auto", "await_shell", "kill_shell", "read_artifact", "list_artifacts", "read_history", "read_stored_result", "update_plan", "exit_plan_mode", "skill", "discover_tools", "call_tool", "await_tool", "cancel_tool", "message_tool", "workspace_info":
		return false
	default:
		return true
	}
}
