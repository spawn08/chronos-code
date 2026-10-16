package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

type failingRuntimeFiles struct {
	storage.Storage
	storage.SessionFileStore
	failIntent atomic.Bool
	failResult atomic.Bool
}

func (s *failingRuntimeFiles) PutFile(ctx context.Context, session, path string, data []byte) error {
	if s.failIntent.Load() {
		return errors.New("intent storage unavailable")
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err == nil && s.failResult.Load() && receipt["status"] == "completed" {
		return errors.New("result storage unavailable")
	}
	return s.SessionFileStore.PutFile(ctx, session, path, data)
}

func runtimeSupervisor(t *testing.T, capacity int) (*toolSupervisor, *failingRuntimeFiles, context.Context) {
	t.Helper()
	a := pipelineAgent(t)
	files := &failingRuntimeFiles{Storage: a.Storage, SessionFileStore: a.Storage.(storage.SessionFileStore)}
	s := &toolSupervisor{runs: map[string]*supervisedTool{}, active: map[string]string{}, gate: make(chan struct{}, capacity), yield: time.Millisecond, store: files}
	t.Cleanup(func() {
		s.Close()
		s.mu.Lock()
		var pending []chan struct{}
		for _, run := range s.runs {
			pending = append(pending, run.done)
		}
		s.mu.Unlock()
		for _, done := range pending {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("supervised test handler did not terminate")
			}
		}
	})
	return s, files, storage.WithSession(context.Background(), "supervised")
}

func TestSupervisorIntentFailurePreventsEffectAndReleasesAdmission(t *testing.T) {
	s, files, ctx := runtimeSupervisor(t, 1)
	files.failIntent.Store(true)
	var calls atomic.Int32
	handler := func(context.Context, map[string]any) (any, error) { calls.Add(1); return "ok", nil }
	if _, err := s.start(ctx, "custom", nil, handler); err == nil {
		t.Fatal("failed intent accepted")
	}
	if calls.Load() != 0 || len(s.gate) != 0 || len(s.runs) != 0 {
		t.Fatal("intent failure invoked an effect or leaked admission")
	}
	files.failIntent.Store(false)
	s.yield = time.Second
	if out, err := s.start(ctx, "custom", nil, handler); err != nil || out != "ok" || calls.Load() != 1 {
		t.Fatalf("retry of unstarted call=%v err=%v calls=%d", out, err, calls.Load())
	}
}

func TestSupervisorJournalIntentFailurePreventsEffect(t *testing.T) {
	_, files, ctx := runtimeSupervisor(t, 1)
	files.failIntent.Store(true)
	a := &agent.Agent{ID: "fail-closed", Storage: files}
	j := &interactiveJournal{a: a}
	if err := j.save(ctx, journalRound{Calls: []journalCall{{ID: "pending", State: "started"}}}); err == nil {
		t.Fatal("failed journal intent accepted")
	}
}

func TestSupervisorHungCancelRetainsCapacityAndLateResult(t *testing.T) {
	s, files, ctx := runtimeSupervisor(t, 1)
	release := make(chan struct{})
	finished := make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }); <-finished }
	t.Cleanup(finish)
	cancelled := make(chan struct{})
	var calls atomic.Int32
	out, err := s.start(ctx, "custom_hung", nil, func(ctx context.Context, _ map[string]any) (any, error) {
		defer close(finished)
		calls.Add(1)
		<-ctx.Done()
		close(cancelled)
		<-release
		return "usable late finding", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	id := out.(map[string]any)["tool_run_id"].(string)
	out, err = s.await(ctx, id, 0, true)
	if err != nil || out.(map[string]any)["status"] != "cancel_requested" {
		t.Fatalf("cancel=%v err=%v", out, err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("handler did not receive cancellation")
	}
	if _, err := s.start(ctx, "another", nil, func(context.Context, map[string]any) (any, error) {
		t.Error("abandoned capacity reused")
		return nil, nil
	}); err == nil {
		t.Fatal("cancellation released capacity before handler exit")
	}
	duplicate, err := s.start(ctx, "custom_hung", nil, func(context.Context, map[string]any) (any, error) { t.Error("duplicate effect"); return nil, nil })
	if err != nil || duplicate.(map[string]any)["tool_run_id"] != id || calls.Load() != 1 {
		t.Fatalf("duplicate=%v err=%v", duplicate, err)
	}
	other := &toolSupervisor{store: files}
	interrupted, err := other.await(ctx, id, 0, false)
	if err != nil || interrupted.(map[string]any)["status"] != "interrupted" || !strings.Contains(errorFreeJSON(interrupted), "reconcile") {
		t.Fatalf("restart trace=%v err=%v", interrupted, err)
	}
	if running := s.Running(ctx); len(running) != 1 || !strings.Contains(running[0], "idle_ms=") {
		t.Fatalf("missing status/idle: %v", running)
	}
	finish()
	late, err := s.await(ctx, id, time.Second, false)
	if err != nil || late.(map[string]any)["status"] != "completed" || late.(map[string]any)["result"] != "usable late finding" {
		t.Fatalf("late finding=%v err=%v", late, err)
	}
	late, err = other.await(ctx, id, 0, false)
	if err != nil || late.(map[string]any)["result"] != "usable late finding" {
		t.Fatalf("durable late finding=%v err=%v", late, err)
	}
}

func TestSupervisorRequestCancellationDoesNotAbandonProductiveWork(t *testing.T) {
	s, _, ctx := runtimeSupervisor(t, 1)
	request, cancelRequest := context.WithCancel(ctx)
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }); <-handlerDone }
	t.Cleanup(finish)
	args := map[string]any{"nested": map[string]any{"value": "original"}}
	out, err := s.start(request, "productive", args, func(ctx context.Context, args map[string]any) (any, error) {
		defer close(handlerDone)
		<-release
		return args["nested"], ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	args["nested"].(map[string]any)["value"] = "caller changed it"
	cancelRequest()
	id := out.(map[string]any)["tool_run_id"].(string)
	status, err := s.await(ctx, id, 0, false)
	if err != nil || status.(map[string]any)["status"] != "running" {
		t.Fatalf("request cancellation abandoned work: %v %v", status, err)
	}
	finish()
	late, err := s.await(ctx, id, time.Second, false)
	if err != nil || late.(map[string]any)["status"] != "completed" || late.(map[string]any)["error"] != nil || !strings.Contains(errorFreeJSON(late), "original") || strings.Contains(errorFreeJSON(late), "caller changed it") {
		t.Fatalf("request/arguments were not detached: %v %v", late, err)
	}
}

func TestSupervisorResultFailureRetainsOriginalFindingAndReplayGuard(t *testing.T) {
	s, files, ctx := runtimeSupervisor(t, 1)
	files.failResult.Store(true)
	s.yield = time.Second
	var calls atomic.Int32
	handler := func(context.Context, map[string]any) (any, error) { calls.Add(1); return "irreplaceable finding", nil }
	out, err := s.start(ctx, "custom", nil, handler)
	if err != nil || out.(map[string]any)["status"] != "reconciliation_required" || out.(map[string]any)["result"] != "irreplaceable finding" {
		t.Fatalf("failed receipt lost result: %v %v", out, err)
	}
	id := out.(map[string]any)["tool_run_id"].(string)
	if replay, err := s.start(ctx, "custom", nil, handler); err != nil || replay.(map[string]any)["tool_run_id"] != id || calls.Load() != 1 {
		t.Fatalf("failed receipt replayed: %v %v calls=%d", replay, err, calls.Load())
	}
	if recovered, _ := s.await(ctx, id, 0, false); recovered.(map[string]any)["result"] != "irreplaceable finding" {
		t.Fatalf("await lost partial finding: %v", recovered)
	}
	if _, err := s.start(ctx, "another", nil, handler); err == nil {
		t.Fatal("uncommitted receipts did not bound unresolved result retention")
	}
	files.failResult.Store(false)
	if recovered, err := s.await(ctx, id, 0, false); err != nil || recovered.(map[string]any)["status"] != "completed" || recovered.(map[string]any)["result"] != "irreplaceable finding" {
		t.Fatalf("original receipt was not repaired: %v %v", recovered, err)
	}
	if len(s.gate) != 0 || calls.Load() != 1 {
		t.Fatal("receipt repair replayed the effect or retained admission")
	}
}

func TestSupervisorPanicProducesReceiptAndReleasesCapacity(t *testing.T) {
	s, files, ctx := runtimeSupervisor(t, 1)
	s.yield = time.Second
	if _, err := s.start(ctx, "panicking", nil, func(context.Context, map[string]any) (any, error) { panic("fixture panic") }); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panic did not return an observation: %v", err)
	}
	if len(s.gate) != 0 {
		t.Fatal("panic leaked physical concurrency capacity")
	}
	entries, err := files.ListFiles(ctx, "supervised", "harness/runs/")
	if err != nil || len(entries) != 1 {
		t.Fatalf("panic receipt missing: %v %v", entries, err)
	}
	data, err := files.GetFile(ctx, "supervised", entries[0].Path)
	if err != nil || !strings.Contains(string(data), "fixture panic") {
		t.Fatalf("panic receipt=%s %v", data, err)
	}
}

func TestSupervisorConcurrentDuplicatesInvokeOnce(t *testing.T) {
	s, _, ctx := runtimeSupervisor(t, 2)
	release := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(release); <-done })
	var calls atomic.Int32
	handler := func(context.Context, map[string]any) (any, error) {
		defer close(done)
		calls.Add(1)
		<-release
		return "done", nil
	}
	const count = 20
	ids := make(chan string, count)
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			out, err := s.start(ctx, "same", map[string]any{"key": "value"}, handler)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- out.(map[string]any)["tool_run_id"].(string)
		})
	}
	wg.Wait()
	close(ids)
	var expected string
	for id := range ids {
		if expected == "" {
			expected = id
		}
		if id != expected {
			t.Fatal("concurrent duplicate obtained another run")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("handler launched %d times", calls.Load())
	}
}

func TestSupervisorTenantAndSessionIsolation(t *testing.T) {
	s, _, ctx := runtimeSupervisor(t, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(release); <-done })
	out, err := s.start(ctx, "owned", nil, func(context.Context, map[string]any) (any, error) { defer close(done); <-release; return "secret", nil })
	if err != nil {
		t.Fatal(err)
	}
	id := out.(map[string]any)["tool_run_id"].(string)
	for _, foreign := range []context.Context{storage.WithSession(ctx, "other"), storage.WithTenant(ctx, "other")} {
		if _, err := s.await(foreign, id, 0, true); err == nil {
			t.Fatal("foreign owner accessed/cancelled original run")
		}
		if lines := s.Running(foreign); len(lines) != 0 {
			t.Fatalf("foreign status leaked: %v", lines)
		}
	}
}

func TestSupervisorSubagentSteeringAndCancellationPreserveFindings(t *testing.T) {
	s, _, ctx := runtimeSupervisor(t, 1)
	a := pipelineAgent(t)
	handlerDone := make(chan struct{})
	a.Tools.Register(&tool.Definition{Name: "spawn_subagent", Permission: tool.PermAllow, Handler: func(ctx context.Context, _ map[string]any) (any, error) {
		defer close(handlerDone)
		<-ctx.Done()
		return "partial review finding", nil
	}})
	(&Orchestrator{toolSupervisor: s}).installToolSupervision(a)
	spawn, _ := a.Tools.Get("spawn_subagent")
	out, err := spawn.Handler(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	id := out.(map[string]any)["tool_run_id"].(string)
	message, _ := a.Tools.Get("message_tool")
	if _, err := message.Handler(ctx, map[string]any{"tool_run_id": id, "message": "focus on crash safety"}); err != nil {
		t.Fatal(err)
	}
	cancel, _ := a.Tools.Get("cancel_tool")
	if _, err := cancel.Handler(ctx, map[string]any{"tool_run_id": id}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("subagent did not receive cancellation")
	}
	out, err = s.await(ctx, id, time.Second, false)
	if err != nil || out.(map[string]any)["status"] != "completed" || !strings.Contains(errorFreeJSON(out), "partial review finding") || !strings.Contains(errorFreeJSON(out), "focus on crash safety") {
		t.Fatalf("steering/partial finding lost: %v %v", out, err)
	}
}

func TestSupervisorHungCustomHandlerDoesNotBlockSDKContinuation(t *testing.T) {
	s, _, ctx := runtimeSupervisor(t, 1)
	provider := &parallelReadProvider{guardTestProvider: guardTestProvider{id: "gpt-4o"}, toolCalls: []model.ToolCall{{ID: "hung", Name: "custom_hung", Arguments: `{}`}}}
	a := newExecutionTestAgent("liveness", provider)
	a.Storage = s.store
	release := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(release); <-done })
	a.Tools.Register(&tool.Definition{Name: "custom_hung", Permission: tool.PermAllow, Handler: func(context.Context, map[string]any) (any, error) {
		defer close(done)
		<-release
		return "eventual result", nil
	}})
	(&Orchestrator{toolSupervisor: s}).installToolSupervision(a)
	a.Hooks = append(a.Hooks, &interactiveJournal{a: a})
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := a.Chat(bounded, "run the custom handler"); err != nil {
		t.Fatalf("hung handler prevented SDK continuation: %v", err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("model did not continue: %d requests", len(provider.requests))
	}
	var receipt map[string]any
	for _, msg := range provider.requests[1].Messages {
		if msg.ToolCallID == "hung" {
			if err := json.Unmarshal([]byte(msg.Content), &receipt); err != nil {
				t.Fatal(err)
			}
		}
	}
	if receipt["status"] != "running" || receipt["heartbeat_at"] == nil || receipt["idle_ms"] == nil {
		t.Fatalf("model did not receive a recoverable liveness receipt: %v", receipt)
	}
}

func TestSupervisorArgumentsDurableBeforeHandler(t *testing.T) {
	s, files, ctx := runtimeSupervisor(t, 1)
	s.yield = time.Second
	out, err := s.start(ctx, "effect", map[string]any{"destination": "remote", "nested": map[string]any{"value": "original"}}, func(ctx context.Context, _ map[string]any) (any, error) {
		entries, err := files.ListFiles(ctx, "supervised", "harness/runs/")
		if err != nil || len(entries) != 1 {
			return nil, errors.New("effect ran without durable run intent")
		}
		data, err := files.GetFile(ctx, "supervised", entries[0].Path)
		if err != nil || !strings.Contains(string(data), `"destination":"remote"`) || !strings.Contains(string(data), `"value":"original"`) {
			return nil, errors.New("effect arguments were not durable before execution")
		}
		return "intent verified", nil
	})
	if err != nil || out != "intent verified" {
		t.Fatalf("durable intent=%v err=%v", out, err)
	}
}

func TestSupervisorCompletedCallIDDoesNotReplayAcrossHosts(t *testing.T) {
	s, files, ctx := runtimeSupervisor(t, 1)
	s.yield = time.Second
	provider := &parallelReadProvider{guardTestProvider: guardTestProvider{id: "gpt-4o"}, toolCalls: []model.ToolCall{{ID: "original-id", Name: "effect", Arguments: `{}`}}}
	a := newExecutionTestAgent("call-id", provider)
	a.Storage = s.store
	var originalContext context.Context
	var calls atomic.Int32
	handler := func(ctx context.Context, _ map[string]any) (any, error) {
		originalContext = ctx
		calls.Add(1)
		return "original effect receipt", nil
	}
	a.Tools.Register(&tool.Definition{Name: "effect", Permission: tool.PermAllow, Handler: handler})
	(&Orchestrator{toolSupervisor: s}).installToolSupervision(a)
	if _, err := a.Chat(ctx, "execute"); err != nil {
		t.Fatal(err)
	}
	other := &toolSupervisor{runs: map[string]*supervisedTool{}, active: map[string]string{}, gate: make(chan struct{}, 1), store: files}
	t.Cleanup(other.Close)
	out, err := other.start(context.WithoutCancel(originalContext), "effect", map[string]any{}, handler)
	if err != nil || out.(map[string]any)["status"] != "completed" || out.(map[string]any)["result"] != "original effect receipt" || calls.Load() != 1 {
		t.Fatalf("completed call replayed: %v err=%v calls=%d", out, err, calls.Load())
	}
}
