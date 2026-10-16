package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

func TestInteractiveJournalSDKIntentAndPerResultDurable(t *testing.T) {
	provider := &parallelReadProvider{guardTestProvider: guardTestProvider{id: "gpt-4o"}, toolCalls: []model.ToolCall{
		{ID: "first", Name: "first", Arguments: `{}`},
		{ID: "second", Name: "second", Arguments: `{}`},
	}}
	a := newExecutionTestAgent("journal", provider)
	a.Storage = pipelineAgent(t).Storage
	j := &interactiveJournal{a: a, root: t.TempDir()}
	a.Hooks = append(a.Hooks, j)
	for index, name := range []string{"first", "second"} {
		a.Tools.Register(&tool.Definition{Name: name, Permission: tool.PermAllow, Handler: func(ctx context.Context, _ map[string]any) (any, error) {
			round, err := j.load(ctx)
			if err != nil || len(round.Calls) != 2 || round.Calls[index].State != "started" {
				return nil, fmt.Errorf("effect ran without durable intent: %+v: %v", round, err)
			}
			if index == 0 && round.Calls[1].State != "planned" {
				return nil, fmt.Errorf("second intent missing: %+v", round)
			}
			if index == 1 && string(round.Calls[0].Result) != `"first receipt"` {
				return nil, fmt.Errorf("first receipt was not independently durable: %+v", round)
			}
			return name + " receipt", nil
		}})
	}
	ctx := storage.WithSession(context.Background(), "intent")
	if _, err := a.Chat(ctx, "execute both"); err != nil {
		t.Fatal(err)
	}
	round, err := j.load(ctx)
	if err != nil || len(round.Calls) != 2 || round.Calls[1].State != "completed" || string(round.Calls[0].Result) != `"first receipt"` || string(round.Calls[1].Result) != `"second receipt"` {
		t.Fatalf("receipts=%+v err=%v", round, err)
	}
}

func TestInteractiveJournalRestoresCompletedRoundAfterCrash(t *testing.T) {
	a := pipelineAgent(t)
	j := &interactiveJournal{a: a, root: t.TempDir()}
	ctx := agent.WithRunIdentity(storage.WithSession(context.Background(), "complete"), agent.RunIdentity{InvocationID: "old"})
	req := &model.ChatRequest{Model: "gpt-4o", Messages: []model.Message{{Role: model.RoleUser, Content: "original request"}}}
	resp := &model.ChatResponse{ToolCalls: []model.ToolCall{{ID: "done", Name: "custom", Arguments: `{}`}}}
	if err := j.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter, Input: req, Output: resp}); err != nil {
		t.Fatal(err)
	}
	if err := j.After(ctx, &hooks.Event{Type: hooks.EventToolCallAfter, Name: "custom", Output: "durable result", Metadata: map[string]any{"interactive_call_id": "done"}}); err != nil {
		t.Fatal(err)
	}
	live := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleSystem, Content: "current policy"}, {Role: model.RoleUser, Content: "continue"}}}
	if err := j.Before(agent.WithRunIdentity(ctx, agent.RunIdentity{InvocationID: "new"}), &hooks.Event{Type: hooks.EventModelCallBefore, Input: live}); err != nil {
		t.Fatal(err)
	}
	if len(live.Messages) != 5 || live.Messages[0].Content != "current policy" || live.Messages[3].ToolCallID != "done" || live.Messages[3].Content != `"durable result"` || live.Messages[4].Content != "continue" {
		t.Fatalf("completed round not restored: %+v", live.Messages)
	}
}

func TestInteractiveJournalUnknownEffectBlocksAcrossRoundsAndDispatch(t *testing.T) {
	a := pipelineAgent(t)
	j := &interactiveJournal{a: a, root: t.TempDir()}
	ctx := storage.WithSession(context.Background(), "uncertain")
	args := map[string]any{"destination": "remote", "value": 1}
	if err := j.Before(ctx, &hooks.Event{Type: hooks.EventToolCallBefore, Name: "custom_mutate", Input: args}); err != nil {
		t.Fatal(err)
	}
	if err := j.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter, Input: &model.ChatRequest{}, Output: &model.ChatResponse{ToolCalls: []model.ToolCall{{ID: "unrelated", Name: "file_read", Arguments: `{}`}}}}); err != nil {
		t.Fatal(err)
	}
	for _, evt := range []*hooks.Event{
		{Type: hooks.EventToolCallBefore, Name: "custom_mutate", Input: args},
		{Type: hooks.EventToolCallBefore, Name: "call_tool", Input: map[string]any{"name": "custom_mutate", "arguments": args}},
	} {
		if err := j.Before(ctx, evt); err == nil || !strings.Contains(err.Error(), "unresolved") {
			t.Fatalf("pending mutation allowed through %s: %v", evt.Name, err)
		}
	}
}

type interruptOuterReceipt struct{}

func (interruptOuterReceipt) Before(context.Context, *hooks.Event) error { return nil }
func (interruptOuterReceipt) After(_ context.Context, evt *hooks.Event) error {
	if evt.Type == hooks.EventToolCallAfter && evt.Name == "call_tool" {
		return errors.New("simulated host interruption")
	}
	return nil
}

func TestInteractiveJournalDeferredWriteCrashRecovery(t *testing.T) {
	root := t.TempDir()
	provider := &parallelReadProvider{guardTestProvider: guardTestProvider{id: "gpt-4o"}, toolCalls: []model.ToolCall{{ID: "outer", Name: "call_tool", Arguments: `{"name":"file_write","arguments":{"path":"effect.txt","content":"once"}}`}}}
	a := newExecutionTestAgent("deferred-journal", provider)
	a.Storage = pipelineAgent(t).Storage
	j := &interactiveJournal{a: a, root: root}
	a.Hooks = append(a.Hooks, j, interruptOuterReceipt{})
	a.Tools.Register(&tool.Definition{Name: "file_write", Permission: tool.PermAllow, Handler: func(_ context.Context, args map[string]any) (any, error) {
		return "written", os.WriteFile(filepath.Join(root, args["path"].(string)), []byte(args["content"].(string)), 0o600)
	}})
	(&Orchestrator{cfg: &config.Config{Tools: config.ToolsConfig{DeferredSchemas: true}}}).installDeferredTools(a)
	ctx := agent.WithRunIdentity(storage.WithSession(context.Background(), "nested"), agent.RunIdentity{InvocationID: "old"})
	if _, err := a.Chat(ctx, "write once"); err == nil {
		t.Fatal("interruption not propagated")
	}
	round, err := j.load(ctx)
	if err != nil || len(round.Calls) != 1 || round.Calls[0].State != "started" || round.Calls[0].Dispatch == nil || round.Calls[0].Dispatch.State != "completed" {
		t.Fatalf("nested receipt=%+v err=%v", round, err)
	}
	live := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "continue"}}}
	if err := j.Before(agent.WithRunIdentity(ctx, agent.RunIdentity{InvocationID: "new"}), &hooks.Event{Type: hooks.EventModelCallBefore, Input: live}); err != nil {
		t.Fatal(err)
	}
	var receipts int
	for _, msg := range live.Messages {
		if msg.Role == model.RoleTool {
			receipts++
			if msg.ToolCallID != "outer" || msg.Name != "call_tool" || msg.Content != `"written"` {
				t.Fatalf("invalid native dispatch group: %+v", msg)
			}
		}
	}
	if receipts != 1 {
		t.Fatalf("native group has %d receipts", receipts)
	}
}

func TestInteractiveJournalCancelledResultStillDurable(t *testing.T) {
	a := pipelineAgent(t)
	j := &interactiveJournal{a: a}
	ctx, cancel := context.WithCancel(storage.WithSession(context.Background(), "cancelled"))
	evt := &hooks.Event{Type: hooks.EventToolCallBefore, Name: "custom", Input: map[string]any{}}
	if err := j.Before(ctx, evt); err != nil {
		t.Fatal(err)
	}
	cancel()
	evt.Type, evt.Output = hooks.EventToolCallAfter, "last result"
	if err := j.After(ctx, evt); err != nil {
		t.Fatal(err)
	}
	round, err := j.load(context.WithoutCancel(ctx))
	if err != nil || string(round.Calls[0].Result) != `"last result"` {
		t.Fatalf("cancelled observation lost: %+v %v", round, err)
	}
}

func TestInteractiveJournalConcurrentReceiptsPreserved(t *testing.T) {
	a := pipelineAgent(t)
	j := &interactiveJournal{a: a}
	ctx := storage.WithSession(context.Background(), "parallel-receipts")
	const count = 20
	resp := &model.ChatResponse{}
	for i := range count {
		resp.ToolCalls = append(resp.ToolCalls, model.ToolCall{ID: fmt.Sprint(i), Name: "custom", Arguments: `{}`})
	}
	if err := j.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter, Input: &model.ChatRequest{}, Output: resp}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			if err := j.After(ctx, &hooks.Event{Type: hooks.EventToolCallAfter, Name: "custom", Output: i, Metadata: map[string]any{"interactive_call_id": fmt.Sprint(i)}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	round, err := j.load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, call := range round.Calls {
		var result int
		if err := json.Unmarshal(call.Result, &result); err != nil || result != i || call.State != "completed" {
			t.Fatalf("lost receipt %d: %+v %v", i, call, err)
		}
	}
}

func TestInteractiveJournalBackgroundFinalPreservesRecoveryAndLateReceipt(t *testing.T) {
	s, _, ctx := runtimeSupervisor(t, 1)
	a := &agent.Agent{ID: "pending-final", Storage: s.store}
	j := &interactiveJournal{a: a}
	ctx = agent.WithRunIdentity(ctx, agent.RunIdentity{InvocationID: "original"})
	if err := j.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter, Input: &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "delegate"}}}, Output: &model.ChatResponse{ToolCalls: []model.ToolCall{{ID: "pending", Name: "custom", Arguments: `{}`}}}}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }); <-done }
	t.Cleanup(finish)
	out, err := s.start(ctx, "custom", nil, func(context.Context, map[string]any) (any, error) {
		defer close(done)
		<-release
		return "late durable result", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	id := out.(map[string]any)["tool_run_id"].(string)
	if err := j.After(ctx, &hooks.Event{Type: hooks.EventToolCallAfter, Name: "custom", Output: out, Metadata: map[string]any{"interactive_call_id": "pending"}}); err != nil {
		t.Fatal(err)
	}
	if err := j.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter, Input: &model.ChatRequest{}, Output: &model.ChatResponse{Content: "independent work complete"}}); err != nil {
		t.Fatal(err)
	}
	round, err := j.load(ctx)
	if err != nil || round.Recovered || round.Calls[0].State != "running" {
		t.Fatalf("final discarded pending effect: %+v %v", round, err)
	}
	resumed := agent.WithRunIdentity(ctx, agent.RunIdentity{InvocationID: "resumed"})
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "continue"}}}
	if err := j.Before(resumed, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errorFreeJSON(req.Messages), id) || !strings.Contains(errorFreeJSON(req.Messages), "await_tool") {
		t.Fatalf("resume lost pending handle: %+v", req.Messages)
	}
	// A later native round must not discard the pending original observation.
	if err := j.After(resumed, &hooks.Event{Type: hooks.EventModelCallAfter, Input: req, Output: &model.ChatResponse{ToolCalls: []model.ToolCall{{ID: "later", Name: "file_read", Arguments: `{}`}}}}); err != nil {
		t.Fatal(err)
	}
	if err := j.After(resumed, &hooks.Event{Type: hooks.EventToolCallAfter, Name: "file_read", Output: "read result", Metadata: map[string]any{"interactive_call_id": "later"}}); err != nil {
		t.Fatal(err)
	}
	if err := j.After(resumed, &hooks.Event{Type: hooks.EventModelCallAfter, Input: &model.ChatRequest{}, Output: &model.ChatResponse{Content: "later work complete"}}); err != nil {
		t.Fatal(err)
	}
	finish()
	if _, err := s.await(ctx, id, time.Second, false); err != nil {
		t.Fatal(err)
	}
	latest := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "collect the finding"}}}
	if err := j.Before(agent.WithRunIdentity(ctx, agent.RunIdentity{InvocationID: "latest"}), &hooks.Event{Type: hooks.EventModelCallBefore, Input: latest}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errorFreeJSON(latest.Messages), "late durable result") {
		t.Fatalf("resume lost completed background finding after later rounds: %+v", latest.Messages)
	}
}
