package orchestrator

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

// gpt-4 has an 8192-token window; the guard reserves 15% for output.
var gpt4GuardBudget = func() int {
	limit := 8192
	return limit - (limit - int(float64(limit)*(1-contextGuardMargin)))
}()

func guardToolRound(n, words int) []model.Message {
	id := fmt.Sprintf("call-%d", n)
	return []model.Message{
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: id, Name: "read", Arguments: "{}"}}},
		{Role: model.RoleTool, ToolCallID: id, Content: strings.Repeat(fmt.Sprintf("r%d ", n), words)},
	}
}

func guardHistory(rounds, words int) []model.Message {
	messages := []model.Message{{Role: model.RoleSystem, Content: "system"}, {Role: model.RoleUser, Content: "do the task"}}
	for i := 0; i < rounds; i++ {
		messages = append(messages, guardToolRound(i, words)...)
	}
	return messages
}

func runGuard(t *testing.T, guard *contextGuardHook, ctx context.Context, messages []model.Message) []model.Message {
	t.Helper()
	req := &model.ChatRequest{Model: "gpt-4", Messages: append([]model.Message(nil), messages...)}
	if err := guard.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatalf("Before() error = %v", err)
	}
	if got := (guardTokenCounter{model.NewTokenCounter("gpt-4")}).CountTokens(req.Messages); got > gpt4GuardBudget {
		t.Fatalf("request has %d tokens, over the %d-token budget", got, gpt4GuardBudget)
	}
	if req.Messages[1].Content != "do the task" {
		t.Fatalf("current task lost: %v", rolesOf(req.Messages))
	}
	return req.Messages
}

// Once trimming starts, consecutive tool rounds must send the same prefix:
// a provider prompt cache only matches byte-identical earlier messages.
func TestContextGuardKeepsTrimmedPrefixStableAcrossCalls(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0)
	ctx := storage.WithSession(context.Background(), "s1")
	history := guardHistory(12, 600)
	previous := runGuard(t, guard, ctx, history)
	if len(previous) == len(history) && reflect.DeepEqual(previous, history) {
		t.Fatal("history over budget was not trimmed")
	}
	for call := 0; call < 5; call++ {
		history = append(history, guardToolRound(100+call, 40)...)
		got := runGuard(t, guard, ctx, history)
		if len(got) != len(previous)+2 || !reflect.DeepEqual(got[:len(previous)], previous) {
			t.Fatalf("call %d changed the already-sent prefix (%d -> %d messages)", call, len(previous), len(got))
		}
		previous = got
	}
}

// When the reused prefix plus new rounds no longer fits, the guard rebuilds
// the trim instead of exceeding the budget.
func TestContextGuardRebuildsTrimWhenReuseOverflows(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0)
	ctx := storage.WithSession(context.Background(), "s1")
	history := guardHistory(12, 600)
	first := runGuard(t, guard, ctx, history)
	rebuilt := false
	for call := 0; call < 15; call++ {
		history = append(history, guardToolRound(200+call, 600)...)
		got := runGuard(t, guard, ctx, history)
		if len(got) < len(first) || !reflect.DeepEqual(got[:len(first)], first) {
			rebuilt = true
		}
	}
	if !rebuilt {
		t.Fatal("growing history never forced a rebuild of the trimmed prefix")
	}
}

// The per-call task plan is a trailing Uncached message that changes every
// call; it must not defeat reuse and must carry the current content.
func TestContextGuardReuseIgnoresUncachedTail(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0)
	ctx := storage.WithSession(context.Background(), "s1")
	history := guardHistory(12, 600)
	withPlan := func(messages []model.Message, plan string) []model.Message {
		return append(append([]model.Message(nil), messages...), model.Message{Role: model.RoleSystem, Content: taskPlanHeader + plan, Uncached: true})
	}
	previous := runGuard(t, guard, ctx, withPlan(history, "step 1"))
	history = append(history, guardToolRound(300, 40)...)
	got := runGuard(t, guard, ctx, withPlan(history, "step 2"))
	stable := previous[:len(previous)-1]
	if !reflect.DeepEqual(got[:len(stable)], stable) {
		t.Fatal("changing the uncached plan changed the cached prefix")
	}
	if last := got[len(got)-1]; !last.Uncached || last.Content != taskPlanHeader+"step 2" {
		t.Fatalf("last message = %+v, want the current uncached plan", last)
	}
}

// A changed earlier message (for example after SDK compaction) or another
// session must never receive a stale remembered trim.
func TestContextGuardDoesNotReuseTrimForChangedHistoryOrOtherSession(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0)
	history := guardHistory(12, 600)
	runGuard(t, guard, storage.WithSession(context.Background(), "s1"), history)

	// The first tool round was compacted away, so the remembered base no
	// longer prefixes the request.
	changed := append(append([]model.Message(nil), history[:2]...), history[4:]...)
	changed = append(changed, guardToolRound(400, 40)...)
	got := runGuard(t, guard, storage.WithSession(context.Background(), "s1"), changed)
	for _, m := range got {
		if m.ToolCallID == "call-0" {
			t.Fatal("reused a trim built from history that no longer exists")
		}
	}

	other := guardHistory(12, 600)
	other[len(other)-1].Content = strings.Repeat("other ", 600)
	got = runGuard(t, guard, storage.WithSession(context.Background(), "s2"), other)
	if last := got[len(got)-1]; last.ToolCallID != "call-11" || !strings.Contains(last.Content, "other") {
		t.Fatalf("session s2 received another session's trim: %+v", last)
	}
}

// Subagents share the parent's session but run under their own invocation;
// interleaved calls must not evict each other's remembered trim.
func TestContextGuardKeepsSeparateTrimsPerInvocation(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0)
	session := storage.WithSession(context.Background(), "s1")
	parentCtx := agent.WithRunIdentity(session, agent.RunIdentity{RoleID: "chronos-code", InvocationID: "parent"})
	childCtx := agent.WithRunIdentity(session, agent.RunIdentity{RoleID: "coder", InvocationID: "child", ParentInvocationID: "parent"})
	parent, child := guardHistory(12, 600), guardHistory(12, 600)
	for i := range child {
		if child[i].Role == model.RoleTool {
			child[i].Content = strings.Repeat("c ", 600)
		}
	}
	prevParent := runGuard(t, guard, parentCtx, parent)
	prevChild := runGuard(t, guard, childCtx, child)
	for call := 0; call < 4; call++ {
		parent = append(parent, guardToolRound(500+call, 40)...)
		child = append(child, guardToolRound(600+call, 40)...)
		gotParent := runGuard(t, guard, parentCtx, parent)
		gotChild := runGuard(t, guard, childCtx, child)
		if !reflect.DeepEqual(gotParent[:len(prevParent)], prevParent) {
			t.Fatalf("call %d: parent prefix changed after interleaved subagent call", call)
		}
		if !reflect.DeepEqual(gotChild[:len(prevChild)], prevChild) {
			t.Fatalf("call %d: subagent prefix changed after interleaved parent call", call)
		}
		prevParent, prevChild = gotParent, gotChild
	}
}

// Provider-reported prompt sizes recalibrate the budget: when the provider
// counts 30% more tokens than the local counter, a request that fit the
// uncalibrated budget is trimmed, and a well-behaved counter leaves it alone.
func TestContextGuardCalibratesBudgetFromReportedUsage(t *testing.T) {
	call := func(guard *contextGuardHook, messages []model.Message, reportScale float64) []model.Message {
		req := &model.ChatRequest{Model: "gpt-4", Messages: append([]model.Message(nil), messages...)}
		evt := &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}
		if err := guard.Before(context.Background(), evt); err != nil {
			t.Fatalf("Before() error = %v", err)
		}
		estimate := evt.Metadata[guardEstimateMetadataKey].(guardEstimate).tokens
		evt.Type = hooks.EventModelCallAfter
		evt.Output = &model.ChatResponse{UsageKnown: true, Usage: model.Usage{PromptTokens: int(float64(estimate) * reportScale)}}
		if err := guard.After(context.Background(), evt); err != nil {
			t.Fatalf("After() error = %v", err)
		}
		return req.Messages
	}
	size := func(messages []model.Message) int {
		return (guardTokenCounter{model.NewTokenCounter("gpt-4")}).CountTokens(messages)
	}
	fits := guardHistory(5, 600) // under the uncalibrated budget, over it at 1.3x
	if got := call(newContextGuardHook("gpt-4", 0), fits, 1.0); size(got) != size(fits) {
		t.Fatalf("uncalibrated guard trimmed a fitting request: %d -> %d tokens", size(fits), size(got))
	}

	accurate := newContextGuardHook("gpt-4", 0)
	undercounted := newContextGuardHook("gpt-4", 0)
	for i := 0; i < 8; i++ {
		call(accurate, fits, 1.0)
		call(undercounted, fits, 1.3)
	}
	if got := call(accurate, fits, 1.0); size(got) != size(fits) {
		t.Fatalf("accurate counter trimmed: %d -> %d tokens", size(fits), size(got))
	}
	if got := call(undercounted, fits, 1.3); float64(size(got))*1.3 > float64(gpt4GuardBudget) {
		t.Fatalf("guard ignored a 1.3x provider count: %d local tokens is %.0f provider tokens, over %d", size(got), float64(size(got))*1.3, gpt4GuardBudget)
	}
}

// Standing guidance in the system prompt outlives old conversation turns:
// dropping it invalidates the provider cache and loses project rules.
func TestContextGuardDropsOldTurnsBeforeProjectInstructions(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0)
	docs := model.Message{Role: model.RoleSystem, Content: "<project_instructions>\n" + strings.Repeat("rule ", 300)}
	// Assistant text is not capped like tool output, so only dropping the old
	// turn can bring the request under budget.
	messages := []model.Message{{Role: model.RoleSystem, Content: "system"}, docs,
		{Role: model.RoleUser, Content: "old task"},
		{Role: model.RoleAssistant, Content: strings.Repeat("old answer ", 4000)},
		{Role: model.RoleUser, Content: "current task"}}
	messages = append(messages, guardToolRound(99, 40)...)
	req := &model.ChatRequest{Model: "gpt-4", Messages: messages}
	if err := guard.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatalf("Before() error = %v", err)
	}
	var keptDocs, keptOldTask bool
	for _, m := range req.Messages {
		keptDocs = keptDocs || m.Content == docs.Content
		keptOldTask = keptOldTask || strings.HasPrefix(m.Content, "old answer")
	}
	if !keptDocs {
		t.Fatal("project instructions were dropped while trimming old turns would have fit")
	}
	if keptOldTask {
		t.Fatal("old turn kept: the request was not trimmed as expected")
	}
}

// A trim edits earlier turns: on models that bind thinking to the
// conversation, thinking kept from before the trim must not be replayed,
// while thinking from rounds after the trim is kept.
func TestContextGuardStripsBoundThinkingOnlyAtTrim(t *testing.T) {
	thinking := []map[string]any{{"type": "thinking", "thinking": "", "signature": "sig"}}
	withThinking := func(messages []model.Message) []model.Message {
		for i := range messages {
			if messages[i].Role == model.RoleAssistant {
				messages[i].ProviderState = thinking
			}
		}
		return messages
	}
	guard := newContextGuardHook("claude-opus-5-5", 0, contextGuardOptions{ContextLimit: 8192})
	ctx := storage.WithSession(context.Background(), "s1")
	call := func(messages []model.Message) []model.Message {
		req := &model.ChatRequest{Model: "claude-opus-5-5", Messages: append([]model.Message(nil), messages...)}
		if err := guard.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
			t.Fatalf("Before() error = %v", err)
		}
		return req.Messages
	}
	history := withThinking(guardHistory(12, 600))
	first := call(history)
	for _, m := range first {
		if m.ProviderState != nil {
			t.Fatal("thinking from before the trim was replayed")
		}
	}
	history = append(history, withThinking(guardToolRound(700, 40))...)
	second := call(history)
	if !reflect.DeepEqual(second[:len(first)], first) {
		t.Fatal("reused trim changed the already-sent prefix")
	}
	if second[len(first)].ProviderState == nil {
		t.Fatal("thinking from a round after the trim was stripped")
	}
}

// Each user turn is a new top-level run with a fresh invocation ID; the trim
// must survive it, or every turn re-trims and rewrites the cached history.
func TestContextGuardKeepsTrimAcrossTopLevelTurns(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0)
	session := storage.WithSession(context.Background(), "s1")
	turn := func(id string) context.Context {
		return agent.WithRunIdentity(session, agent.RunIdentity{RoleID: "chronos-code", InvocationID: id})
	}
	history := guardHistory(12, 600)
	first := runGuard(t, guard, turn("turn-1"), history)
	history = append(history, model.Message{Role: model.RoleUser, Content: "next"}, model.Message{Role: model.RoleAssistant, Content: "ok"})
	second := runGuard(t, guard, turn("turn-2"), history)
	if !reflect.DeepEqual(second[:len(first)], first) {
		t.Fatal("a new user turn rebuilt the trim and changed the sent prefix")
	}
}

// Overflow evicts the least recently used conversation, not every one.
func TestContextGuardEvictsLeastRecentlyUsedTrim(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0)
	history := guardHistory(12, 600)
	ctxFor := func(i int) context.Context {
		return storage.WithSession(context.Background(), fmt.Sprintf("s%d", i))
	}
	for i := 0; i < maxContextTrims; i++ {
		runGuard(t, guard, ctxFor(i), history)
	}
	runGuard(t, guard, ctxFor(0), append(append([]model.Message(nil), history...), guardToolRound(900, 10)...)) // touch s0
	runGuard(t, guard, ctxFor(maxContextTrims), history)
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if len(guard.trims) != maxContextTrims {
		t.Fatalf("remembered %d trims, want %d", len(guard.trims), maxContextTrims)
	}
	if guard.trims[contextTrimKey(ctxFor(0))] == nil {
		t.Fatal("evicted a recently used conversation")
	}
	if guard.trims[contextTrimKey(ctxFor(1))] != nil {
		t.Fatal("kept the least recently used conversation")
	}
}

// Some providers' prompt counts omit tokens (KV-cached prefixes on Ollama or
// llama.cpp); an under-report must never enlarge the budget.
func TestContextGuardCalibrationNeverEnlargesBudget(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0)
	for i := 0; i < 10; i++ {
		guard.calibrate("gpt-4", 5000, 1000)
	}
	if got := guard.tokenScale("gpt-4"); got != 1 {
		t.Fatalf("scale after under-reports = %v, want 1", got)
	}
	guard.calibrate("gpt-4", 5000, 10000)
	if got := guard.tokenScale("gpt-4"); got <= 1 || got >= 2 {
		t.Fatalf("scale after one 2x report = %v, want a smoothed value in (1, 2)", got)
	}
}

type servedWindowProvider struct {
	outputTokensProvider
	window int
}

func (p servedWindowProvider) ContextWindow(context.Context) (int, bool) { return p.window, true }

// A provider reporting a smaller served window (a local server's context
// setting) caps the budget below the configured ceiling.
func TestContextGuardCapsBudgetAtServedWindow(t *testing.T) {
	guard := newContextGuardHook("local-model", 0, contextGuardOptions{ContextLimit: 128000})
	history := guardHistory(12, 600)
	req := &model.ChatRequest{Model: "local-model", MaxTokens: 512, Messages: append([]model.Message(nil), history...)}
	evt := &hooks.Event{Type: hooks.EventModelCallBefore, Input: req,
		Metadata: map[string]any{"provider": servedWindowProvider{outputTokensProvider{name: "ollama", model: "local-model"}, 4096}}}
	if err := guard.Before(context.Background(), evt); err != nil {
		t.Fatalf("Before() error = %v", err)
	}
	if got := (guardTokenCounter{model.NewTokenCounter("local-model")}).CountTokens(req.Messages); got > 4096 {
		t.Fatalf("request has %d tokens, over the 4096-token served window", got)
	}
}
