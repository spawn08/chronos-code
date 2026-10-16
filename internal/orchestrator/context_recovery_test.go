package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

func TestRecoveryCompactionRetainsLateToolFindingThroughSDK(t *testing.T) {
	a := pipelineAgent(t)
	ctx := storage.WithSession(context.Background(), "recovery")
	provider := &executionTestProvider{name: "Goal: finish audit. Findings are in the referenced artifacts.", modelID: "gpt-4o"}
	a.Model = provider
	a.ContextCfg.PreserveRecentTurns = 1
	o := &Orchestrator{store: a.Storage, cfg: &config.Config{}}
	o.installArtifacts(a)
	o.installWorkingContext(a)
	calls := 0
	finding := strings.Repeat("ordinary output ", 160) + "CRITICAL_LATE_FINDING"
	a.Tools.Register(&tool.Definition{Name: "audit", Handler: func(context.Context, map[string]any) (any, error) { calls++; return finding, nil }})
	wrapToolResultCap(a)
	audit, _ := a.Tools.Get("audit")
	result, err := audit.Handler(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	resultJSON, _ := json.Marshal(result)
	id := result.(map[string]any)["artifact_id"].(string)
	if !strings.Contains(string(resultJSON[:min(1000, len(resultJSON))]), id) {
		t.Fatal("artifact reference is outside the SDK summary prefix")
	}
	messages := []model.Message{
		{Role: model.RoleUser, Content: "Audit only; do not edit files."},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "audit-1", Name: "audit", Arguments: "{}"}}},
		{Role: model.RoleTool, Name: "audit", ToolCallID: "audit-1", Content: string(resultJSON)},
		{Role: model.RoleAssistant, Content: "Audit completed."},
		{Role: model.RoleUser, Content: "What was the critical finding?"},
		{Role: model.RoleAssistant, Content: "Checking saved findings."},
	}
	for i, msg := range messages {
		data, _ := json.Marshal(msg)
		var payload map[string]any
		_ = json.Unmarshal(data, &payload)
		if err := a.Storage.AppendEvent(ctx, &storage.Event{ID: fmt.Sprintf("history-%d", i), SessionID: "recovery", SeqNum: int64(i + 1), Type: "chat_message", Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.CompactSession(ctx, "recovery"); err != nil {
		t.Fatal(err)
	}
	prompt := provider.request(0)
	encoded, _ := json.Marshal(prompt.Messages)
	if !strings.Contains(string(encoded), id) || strings.Contains(string(encoded), "CRITICAL_LATE_FINDING") {
		t.Fatalf("fixture did not exercise SDK prefix truncation: %s", encoded)
	}
	for _, term := range []string{"constraints", "versions/hashes", "accepted checks", "failed hypotheses", "active tasks/shells", "next action"} {
		if !strings.Contains(prompt.Messages[0].Content, term) {
			t.Errorf("summary contract missing %q", term)
		}
	}
	events, err := a.Storage.ListEvents(ctx, "recovery", 0)
	if err != nil {
		t.Fatal(err)
	}
	summary := events[len(events)-1].Payload.(map[string]any)["summary"].(string)
	if !strings.Contains(summary, "compaction:") || !strings.Contains(summary, "summary:") {
		t.Fatalf("no durable recovery receipt: %s", summary)
	}
	restarted := &Orchestrator{}
	restarted.installArtifacts(a)
	read, _ := a.Tools.Get("read_artifact")
	recovered, err := read.Handler(ctx, map[string]any{"artifact_id": id, "byte_offset": -500, "max_bytes": 500})
	if err != nil || !strings.Contains(recovered.(map[string]any)["content"].(string), "CRITICAL_LATE_FINDING") {
		t.Fatalf("finding not recoverable: %v, %v", recovered, err)
	}
	if calls != 1 {
		t.Fatalf("audit rerun %d times", calls)
	}
	if _, err := read.Handler(storage.WithSession(ctx, "another-session"), map[string]any{"artifact_id": id}); err == nil {
		t.Fatal("cross-session artifact read allowed")
	}
}

func TestRecoverySummaryPartialFragmentIsDurable(t *testing.T) {
	a := pipelineAgent(t)
	ctx := storage.WithSession(context.Background(), "partial")
	h := &workingContextHook{o: &Orchestrator{store: a.Storage}}
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleSystem, Content: "You are a conversation summarizer."}, {Role: model.RoleUser, Content: "goal and unresolved work"}}}
	if err := h.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	resp := &model.ChatResponse{Content: "Goal: investigate. Open items: interrupted", StopReason: model.StopReasonMaxTokens}
	if err := h.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter, Input: req, Output: resp}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "partial state") || !strings.Contains(resp.Content, "summary:") {
		t.Fatal(resp.Content)
	}
	files := a.Storage.(storage.SessionFileStore)
	infos, err := files.ListFiles(ctx, "partial", artifactPrefix)
	if err != nil || len(infos) != 2 {
		t.Fatalf("expected source and fragment artifacts: %v %v", infos, err)
	}
	for _, info := range infos {
		data, err := files.GetFile(ctx, "partial", info.Path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(info.Path, "summary:") && (!strings.Contains(string(data), "partial state") || !strings.Contains(string(data), "interrupted")) {
			t.Fatalf("lost partial summary: %s", data)
		}
	}
}

func TestRecoveryTailIsBoundedUncachedAndAnchoredToLatestRawUser(t *testing.T) {
	a := pipelineAgent(t)
	a.ContextPinsFn = func(context.Context) []model.Message {
		return []model.Message{{Role: model.RoleSystem, TurnScoped: true, Content: strings.Repeat("界", 20000)}}
	}
	o := &Orchestrator{cfg: &config.Config{Tools: config.ToolsConfig{LeanContext: true}}}
	o.installWorkingContext(a)
	ctx := storage.WithSession(context.Background(), "tail-session")
	ctx = context.WithValue(ctx, repositoryContextKey{}, strings.Repeat("selected source ", 10000))
	ctx = context.WithValue(ctx, routingContextKey{}, "routing to audit")
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "old topic"}, {Role: model.RoleUser, Content: "latest task"}, {Role: model.RoleUser, Name: "harness-scoped-rules", TurnScoped: true, Content: "instructions"}}}
	if err := a.Hooks.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	tail := req.Messages[len(req.Messages)-1]
	if !tail.Uncached || !tail.TurnScoped || len(tail.Content) > maxWorkingContextBytes || !utf8.ValidString(tail.Content) {
		t.Fatalf("invalid tail: %+v", tail)
	}
	for _, term := range []string{fmt.Sprintf("user_sha256=%x", sha256.Sum256([]byte("latest task"))), "source=turn-recall", "source=repository-selection", "source=routing-advisory", "Partial context block", "not current repository evidence"} {
		if !strings.Contains(tail.Content, term) {
			t.Errorf("tail missing %q", term)
		}
	}
	if req.Messages[1].Content != "latest task" {
		t.Fatal("raw user was annotated")
	}
}

func TestRecoveryGuardDoesNotTreatHarnessTailAsNewUserTurn(t *testing.T) {
	counter := model.NewTokenCounter("gpt-4")
	messages := guardHistory(1, 100)
	messages = append(messages, model.Message{Role: model.RoleAssistant, Content: strings.Repeat("must preserve live work ", 1000)}, model.Message{Role: model.RoleUser, Name: workingContextName, Uncached: true, TurnScoped: true, Content: "annotations"})
	trimmed, _ := trimMessages(counter, messages, 1, 200)
	if len(trimmed) != len(messages) || trimmed[1].Content != "do the task" || trimmed[len(trimmed)-2].Content != messages[len(messages)-2].Content {
		t.Fatal("annotation allowed deletion of the live user's tool progress")
	}
	guard := newContextGuardHook("gpt-4", 0, contextGuardOptions{ContextLimit: 1000})
	req := &model.ChatRequest{Messages: messages}
	var overflow *ContextBudgetOverflowError
	if err := guard.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); !errors.As(err, &overflow) {
		t.Fatalf("expected explicit overflow preserving live task, got %v", err)
	}
}

func TestRecoveryGuardReusesTrimWhenUncachedTailChanges(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0)
	ctx := storage.WithSession(context.Background(), "cache-tail")
	history := guardHistory(12, 600)
	firstInput := append(append([]model.Message(nil), history...), model.Message{Role: model.RoleUser, Name: workingContextName, Uncached: true, TurnScoped: true, Content: "old tail"})
	first := runGuard(t, guard, ctx, firstInput)
	secondInput := append(append([]model.Message(nil), history...), guardToolRound(200, 20)...)
	secondInput = append(secondInput, model.Message{Role: model.RoleUser, Name: workingContextName, Uncached: true, TurnScoped: true, Content: "new tail"})
	second := runGuard(t, guard, ctx, secondInput)
	if !reflect.DeepEqual(second[:len(first)-1], first[:len(first)-1]) || second[len(second)-1].Content != "new tail" {
		t.Fatal("mutable tail rewrote remembered cached prefix")
	}
}

func TestRecoveryGuardHistoricalOutputGetsArtifactBeforeTrim(t *testing.T) {
	a := pipelineAgent(t)
	ctx := storage.WithSession(context.Background(), "historical")
	guard := newContextGuardHook("gpt-4", 0, contextGuardOptions{ContextLimit: 1500, Store: a.Storage})
	full := strings.Repeat("long report ", 3000) + "FINAL_FINDING"
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "review"}, {Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "report", Name: "audit", Arguments: "{}"}}}, {Role: model.RoleTool, Name: "audit", ToolCallID: "report", Content: full}}}
	if err := guard.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	var preview map[string]any
	if err := json.Unmarshal([]byte(req.Messages[2].Content), &preview); err != nil {
		t.Fatal(err)
	}
	id := preview["artifact_id"].(string)
	data, err := a.Storage.(storage.SessionFileStore).GetFile(ctx, "historical", artifactPrefix+id)
	if err != nil || !strings.Contains(string(data), "FINAL_FINDING") {
		t.Fatalf("late finding lost before trim: %s %v", data, err)
	}
}

func TestRecoveryRequestBudgetIncludesSchemasPinsOutputAndCalibration(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0, contextGuardOptions{ContextLimit: 4096})
	guard.calibrate("gpt-4", 5000, 7500)
	req := &model.ChatRequest{Model: "gpt-4", MaxTokens: 1000, ResponseFormat: "json_schema", Metadata: map[string]any{"json_schema": map[string]any{"type": "object", "description": strings.Repeat("format ", 30)}}, Tools: []model.ToolDefinition{{Type: "function", Function: model.FunctionDef{Name: "audit", Description: strings.Repeat("tool ", 50)}}}, Messages: []model.Message{{Role: model.RoleSystem, Name: "policy", Content: "standing policy"}, {Role: model.RoleUser, Content: "task"}}}
	evt := &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}
	if err := guard.Before(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	budget := evt.Metadata[guardBudgetMetadataKey].(map[string]any)
	counter := guardTokenCounter{model.NewTokenCounter("gpt-4")}
	tools, _ := json.Marshal(req.Tools)
	schema, _ := json.Marshal(req.Metadata["json_schema"])
	schemaTokens := counter.CountString(string(tools)) + counter.CountString(string(schema))
	expected := int(float64(4096-1000)/guard.tokenScale("gpt-4")) - schemaTokens
	if budget["schema_tokens"] != schemaTokens || budget["message_budget_tokens"] != expected || budget["output_reserve_tokens"] != 1000 || budget["pinned_tokens"].(int) <= 0 {
		t.Fatalf("incoherent effective budget: %v", budget)
	}
	if err := (requestMetricsHook{}).Before(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	metrics := evt.Metadata["harness_request_metrics"].(map[string]any)
	if !reflect.DeepEqual(metrics["effective_budget"], budget) {
		t.Fatal("metrics did not use guard's effective budget")
	}
}

func TestRecoveryMetricsAttributeOnlyChangedStandingBlocks(t *testing.T) {
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleSystem, Name: "policy", Content: "fixed"}, {Role: model.RoleUser, Content: "latest"}, {Role: model.RoleUser, Name: workingContextName, Uncached: true, Content: "tail one"}}}
	first := requestBlockHashes(req)
	req.Messages[2].Content = "tail two"
	req.Messages = append(req.Messages, model.Message{Role: model.RoleAssistant, Content: "new round"})
	if changed := changedRequestBlocks(first, requestBlockHashes(req)); len(changed) != 0 {
		t.Fatalf("mutable/history append attributed as cache break: %v", changed)
	}
	req.Messages[0].Content = "changed policy"
	if changed := changedRequestBlocks(first, requestBlockHashes(req)); !reflect.DeepEqual(changed, []string{"standing/policy/1"}) {
		t.Fatal(changed)
	}
	req.Tools = []model.ToolDefinition{{Type: "function", Function: model.FunctionDef{Name: "discovered"}}}
	if changed := changedRequestBlocks(first, requestBlockHashes(req)); !reflect.DeepEqual(changed, []string{"standing/policy/1", "tool_schemas"}) {
		t.Fatal(changed)
	}
}

func TestRecoveryGraphPolicyIsNamedStableAndNilSafe(t *testing.T) {
	a := &agent.Agent{}
	if err := installIndexGuidance(map[string]*agent.Agent{"agent": a, "absent": nil}); err != nil {
		t.Fatal(err)
	}
	first := a.ContextPinsFn(context.Background())
	second := a.ContextPinsFn(context.Background())
	if !reflect.DeepEqual(first, second) || len(first) != 1 || first[0].Name != "harness-index-guidance" || first[0].Uncached || first[0].TurnScoped {
		t.Fatalf("guidance is not a named stable policy: %v", first)
	}
}

func BenchmarkRecoveryRememberedTrim(b *testing.B) {
	guard := newContextGuardHook("gpt-4", 0)
	counter := guardTokenCounter{model.NewTokenCounter("gpt-4")}
	history := guardHistory(12, 600)
	trimmed, dropped := trimMessages(counter, history, 1, gpt4GuardBudget-int(float64(gpt4GuardBudget)*contextGuardHeadroom))
	guard.rememberTrim("benchmark", "gpt-4", history, trimmed, dropped)
	history = append(history, guardToolRound(100, 20)...)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, ok := guard.reuseTrim("benchmark", "gpt-4", gpt4GuardBudget, counter, history); !ok {
			b.Fatal("trim not reused")
		}
	}
}

type recoveryFailingFileStore struct {
	storage.Storage
	storage.SessionFileStore
}

func (recoveryFailingFileStore) PutFile(context.Context, string, string, []byte) error {
	return fmt.Errorf("fixture storage unavailable")
}

func TestRecoveryReductionFailsClosedWhenArtifactWriteFails(t *testing.T) {
	a := pipelineAgent(t)
	ctx := storage.WithSession(context.Background(), "write-failure")
	store := recoveryFailingFileStore{Storage: a.Storage, SessionFileStore: a.Storage.(storage.SessionFileStore)}
	full := strings.Repeat("finding ", maxToolResultBytes)
	result, err := recoverAndCapToolResult(ctx, store, full)
	if err == nil || result != full {
		t.Fatal("artifact write failure reduced unrecoverable content")
	}
	h := &workingContextHook{o: &Orchestrator{store: store}}
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleSystem, Content: "You are a conversation summarizer."}, {Role: model.RoleUser, Content: "history"}}}
	original := append([]model.Message(nil), req.Messages...)
	if err := h.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err == nil || !reflect.DeepEqual(original, req.Messages) {
		t.Fatal("summary input changed despite source artifact failure")
	}
	if _, err := renderContextTail(ctx, nil, []contextTailBlock{{"recall", full}}, store); err == nil {
		t.Fatal("tail reduction ignored artifact write failure")
	}
}

func TestRecoveryErrorBearingToolResultRetainsPartialFindings(t *testing.T) {
	a := pipelineAgent(t)
	ctx := storage.WithSession(context.Background(), "partial-tool")
	failure := errors.New("audit interrupted")
	full := strings.Repeat("partial finding ", 300) + "LATE_PARTIAL_FINDING"
	a.Tools.Register(&tool.Definition{Name: "audit", Handler: func(context.Context, map[string]any) (any, error) { return full, failure }})
	wrapToolResultCap(a)
	def, _ := a.Tools.Get("audit")
	result, err := def.Handler(ctx, nil)
	if !errors.Is(err, failure) {
		t.Fatalf("tool failure lost: %v", err)
	}
	id := result.(map[string]any)["artifact_id"].(string)
	data, err := a.Storage.(storage.SessionFileStore).GetFile(ctx, "partial-tool", artifactPrefix+id)
	if err != nil || !strings.Contains(string(data), "LATE_PARTIAL_FINDING") {
		t.Fatalf("partial findings lost: %s %v", data, err)
	}
}

func TestRecoveryTailTruncationAdvertisesRecoverableBlock(t *testing.T) {
	a := pipelineAgent(t)
	ctx := storage.WithSession(context.Background(), "tail-artifact")
	full := strings.Repeat("remembered state ", 1000) + "LATE_TASK_STATE"
	tail, err := renderContextTail(ctx, []model.Message{{Role: model.RoleUser, Content: "latest"}}, []contextTailBlock{{"turn-recall", full}}, a.Storage)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) > maxWorkingContextBytes || !strings.Contains(tail, "Partial context block") {
		t.Fatal("tail is not bounded/signaled")
	}
	marker := "artifact_id="
	start := strings.Index(tail, marker)
	if start < 0 {
		t.Fatal("partial block has no artifact reference")
	}
	id := strings.SplitN(tail[start+len(marker):], "\n", 2)[0]
	data, err := a.Storage.(storage.SessionFileStore).GetFile(ctx, "tail-artifact", artifactPrefix+id)
	if err != nil || !strings.Contains(string(data), "LATE_TASK_STATE") || !strings.Contains(string(data), "user_sha256") {
		t.Fatalf("partial block not recoverable: %s %v", data, err)
	}
}

func TestRecoveryRuntimeDigestCarriesObservedVersionsAndAcceptedChecks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.py")
	if err := os.WriteFile(path, []byte("value = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runtime, err := newTaskRuntime("recovery-task", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.refreshSourceChanges(true); err != nil {
		t.Fatal(err)
	}
	code := 0
	if _, err := runtime.recordCommand("pytest", execution.CommandTest, &code, execution.TerminalExited, []execution.Scope{{Kind: execution.ScopeExact, Path: "source.py"}}, execution.ProvenanceRuntime, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.ledger.State()
	if err != nil {
		t.Fatal(err)
	}
	digest := runtime.recoveryDigest()
	for _, term := range []string{"Active task: recovery-task", "Observed file version: source.py sha256=", "accepted=true", "command=pytest", "revision="} {
		if !strings.Contains(digest, term) {
			t.Errorf("recovery state missing %q: %s", term, digest)
		}
	}
	after, err := runtime.ledger.State()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("rendering recovery state mutated the ledger")
	}
	if err := os.WriteFile(path, []byte("value = 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runtime.refreshSourceChanges(false); err != nil {
		t.Fatal(err)
	}
	if digest := runtime.recoveryDigest(); !strings.Contains(digest, "accepted=false") || !strings.Contains(digest, "Changed-file version: source.py sha256=") {
		t.Fatalf("stale checks advertised as accepted: %s", digest)
	}
}

func TestRecoveryCacheTraceNamesChangedBlocksNotMutableTail(t *testing.T) {
	a := pipelineAgent(t)
	ctx := storage.WithSession(context.Background(), "cache-attribution")
	h := &workingContextHook{o: &Orchestrator{store: a.Storage}}
	req := &model.ChatRequest{Model: "gpt-4o", Messages: []model.Message{{Role: model.RoleSystem, Name: "policy", Content: "fixed"}, {Role: model.RoleUser, Name: workingContextName, Uncached: true, Content: "tail one"}}}
	event := &hooks.Event{Type: hooks.EventModelCallAfter, Input: req, Output: &model.ChatResponse{Content: "answer"}}
	if err := h.After(ctx, event); err != nil {
		t.Fatal(err)
	}
	req.Messages[1].Content = "tail two"
	if err := h.After(ctx, event); err != nil {
		t.Fatal(err)
	}
	traces, err := a.Storage.ListTraces(ctx, "cache-attribution")
	if err != nil || len(traces) != 0 {
		t.Fatalf("mutable tail created cache break: %v %v", traces, err)
	}
	req.Messages[0].Content = "changed policy"
	if err := h.After(ctx, event); err != nil {
		t.Fatal(err)
	}
	traces, err = a.Storage.ListTraces(ctx, "cache-attribution")
	if err != nil || len(traces) != 1 {
		t.Fatalf("missing cache break: %v %v", traces, err)
	}
	data, _ := json.Marshal(traces[0].Output)
	if !strings.Contains(string(data), "standing/policy/1") {
		t.Fatalf("cache break did not name changed block: %s", data)
	}
}

func TestRecoveryGuardRejectsSchemaOutputExhaustion(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0, contextGuardOptions{ContextLimit: 512})
	req := &model.ChatRequest{MaxTokens: 512, Messages: []model.Message{{Role: model.RoleUser, Content: "task"}}}
	var overflow *ContextBudgetOverflowError
	if err := guard.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); !errors.As(err, &overflow) || overflow.BudgetTokens > 0 {
		t.Fatalf("expected reserve overflow, got %v", err)
	}
}

func BenchmarkRecoveryRebuiltTrim(b *testing.B) {
	counter := guardTokenCounter{model.NewTokenCounter("gpt-4")}
	history := append(guardHistory(12, 600), guardToolRound(100, 20)...)
	target := gpt4GuardBudget - int(float64(gpt4GuardBudget)*contextGuardHeadroom)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		trimMessages(counter, history, 1, target)
	}
}

func TestRecoverySummaryAnnotationDoesNotMutateContinuationPrefix(t *testing.T) {
	h := &workingContextHook{o: &Orchestrator{}}
	original := []model.Message{{Role: model.RoleSystem, Content: "You are a conversation summarizer."}, {Role: model.RoleUser, Content: "history"}}
	for i := 0; i < 3; i++ {
		req := &model.ChatRequest{Messages: original}
		if err := h.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
			t.Fatal(err)
		}
		if strings.Count(req.Messages[0].Content, "Recovery contract:") != 1 || strings.Contains(original[0].Content, "Recovery contract:") {
			t.Fatal("summary policy accumulated in the continuation prefix")
		}
	}
}

func TestRecoveryWorkingTailBoundsExistingMutablePlan(t *testing.T) {
	a := pipelineAgent(t)
	ctx := storage.WithSession(context.Background(), "plan-tail")
	o := &Orchestrator{store: a.Storage, cfg: &config.Config{Tools: config.ToolsConfig{LeanContext: true}}}
	o.installWorkingContext(a)
	plan := taskPlanHeader + strings.Repeat("task step ", 2000) + "LAST_PLAN_STEP"
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "latest task"}, {Role: model.RoleSystem, Uncached: true, Content: plan}}}
	if err := a.Hooks.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 || req.Messages[0].Content != "latest task" || len(req.Messages[1].Content) > maxWorkingContextBytes || !strings.Contains(req.Messages[1].Content, "source=live-task-state") || !strings.Contains(req.Messages[1].Content, "artifact_id=") {
		t.Fatalf("plan not moved to bounded recoverable tail: %v", req.Messages)
	}
}
