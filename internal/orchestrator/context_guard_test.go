package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/apierror"
	"github.com/spawn08/chronos-code/internal/modelinfo"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
)

func TestContextGuardTrimsMessagesOverLimit(t *testing.T) {
	guard := newContextGuardHook("claude-haiku-4-5", 10)

	// claude-haiku-4-5 has a 200k context limit. Build a request that exceeds
	// the effective limit (200k * 0.85).
	largeContent := strings.Repeat("word ", 50000) // ~50k tokens
	req := &model.ChatRequest{
		Model: "claude-haiku-4-5",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "You are a helpful assistant."},
			{Role: model.RoleUser, Content: "first question"},
			{Role: model.RoleAssistant, Content: largeContent},
			{Role: model.RoleUser, Content: "second question"},
			{Role: model.RoleAssistant, Content: largeContent},
			{Role: model.RoleUser, Content: "third question"},
			{Role: model.RoleAssistant, Content: largeContent},
			{Role: model.RoleUser, Content: "fourth question"},
			{Role: model.RoleAssistant, Content: largeContent},
			{Role: model.RoleUser, Content: "current question"},
		},
	}
	evt := &hooks.Event{
		Type:  hooks.EventModelCallBefore,
		Input: req,
	}

	err := guard.Before(context.Background(), evt)
	if err != nil {
		t.Fatalf("Before() error = %v", err)
	}
	if len(req.Messages) >= 10 {
		t.Fatalf("expected messages to be trimmed, got %d messages", len(req.Messages))
	}
	// System message should be preserved.
	if req.Messages[0].Role != model.RoleSystem {
		t.Error("system message was trimmed")
	}
	// Last user message should still be present.
	last := req.Messages[len(req.Messages)-1]
	if last.Content != "current question" {
		t.Errorf("last message = %q, want %q", last.Content, "current question")
	}
}

func TestContextGuardSkipsSmallRequests(t *testing.T) {
	guard := newContextGuardHook("claude-haiku-4-5", 5)

	req := &model.ChatRequest{
		Model: "claude-haiku-4-5",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "You are a helper."},
			{Role: model.RoleUser, Content: "hi"},
		},
	}
	evt := &hooks.Event{
		Type:  hooks.EventModelCallBefore,
		Input: req,
	}

	err := guard.Before(context.Background(), evt)
	if err != nil {
		t.Fatalf("Before() error = %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("small request should not be trimmed, got %d messages", len(req.Messages))
	}
}

func TestContextGuardIgnoresNonModelEvents(t *testing.T) {
	guard := newContextGuardHook("claude-haiku-4-5", 5)
	evt := &hooks.Event{Type: hooks.EventToolCallBefore}
	if err := guard.Before(context.Background(), evt); err != nil {
		t.Fatalf("non-model event should be ignored, got error: %v", err)
	}
}

func TestContextGuardDropsOrphanedToolResults(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 5) // 8192 limit

	// Build messages that are definitely over the 8192 limit so trimming
	// must drop the tool-call + tool-result pair together.
	largeContent := strings.Repeat("token ", 5000) // ~5k tokens each
	req := &model.ChatRequest{
		Model: "gpt-4",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "system"},
			{Role: model.RoleUser, Content: "use tools"},
			{Role: model.RoleAssistant, Content: "", ToolCalls: []model.ToolCall{{ID: "1", Name: "read", Arguments: "{}"}}},
			{Role: model.RoleTool, Content: largeContent, ToolCallID: "1"},
			{Role: model.RoleUser, Content: "follow up"},
			{Role: model.RoleAssistant, Content: largeContent + largeContent},
			{Role: model.RoleUser, Content: "current"},
		},
	}
	evt := &hooks.Event{
		Type:  hooks.EventModelCallBefore,
		Input: req,
	}

	origCount := len(req.Messages)
	err := guard.Before(context.Background(), evt)
	if err != nil {
		t.Fatalf("Before() error = %v", err)
	}
	if len(req.Messages) >= origCount {
		t.Fatalf("expected messages to be trimmed, got %d (was %d)", len(req.Messages), origCount)
	}
	// Complete old turns are dropped, including their call/result pairs.
	for i, m := range req.Messages {
		if m.Role == model.RoleTool && (i == 0 || len(req.Messages[i-1].ToolCalls) == 0) {
			t.Errorf("orphaned tool result at index %d", i)
		}
	}
}

func TestContextGuardKeepsUserTurnAfterToolLoopTrim(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 5) // 8192 limit
	large := strings.Repeat("token ", 5000)
	req := &model.ChatRequest{
		Model: "gpt-4",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "system"},
			{Role: model.RoleUser, Content: "do the task"},
			{Role: model.RoleAssistant, Content: "", ToolCalls: []model.ToolCall{{ID: "1", Name: "read", Arguments: "{}"}}},
			{Role: model.RoleTool, Content: large, ToolCallID: "1"},
			{Role: model.RoleAssistant, Content: "", ToolCalls: []model.ToolCall{{ID: "2", Name: "read", Arguments: "{}"}}},
			{Role: model.RoleTool, Content: large, ToolCallID: "2"},
		},
	}
	err := guard.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req})
	if err != nil {
		t.Fatalf("Before() error = %v", err)
	}
	if !hasUserOrAssistant(req.Messages, 0) {
		t.Fatalf("trimmed request has no user/assistant message: %#v", rolesOf(req.Messages))
	}
	if len(req.Messages) != 6 || req.Messages[1].Content != "do the task" {
		t.Fatalf("lost current task/tool progress: %#v", rolesOf(req.Messages))
	}
	for _, pair := range [][2]int{{2, 3}, {4, 5}} {
		if req.Messages[pair[0]].ToolCalls[0].ID != req.Messages[pair[1]].ToolCallID {
			t.Fatal("tool call/result pairing changed")
		}
	}
}

func rolesOf(messages []model.Message) []string {
	roles := make([]string, len(messages))
	for i, m := range messages {
		roles[i] = m.Role
	}
	return roles
}

func TestContextGuardRejectsUntrimmableRequestOverSafeBudget(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0) // 8192 raw-token limit, 15% output reserve.
	req := &model.ChatRequest{
		Model: "gpt-4",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "system"},
			{Role: model.RoleUser, Content: strings.Repeat("token ", 7600)},
		},
	}
	err := guard.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req})
	if err == nil || !strings.Contains(err.Error(), "safe model budget") {
		t.Fatalf("Before() error = %v, want safe-budget rejection", err)
	}
	var overflow *ContextBudgetOverflowError
	if !errors.As(err, &overflow) {
		t.Fatalf("expected typed overflow, got %T", err)
	}
	classified := apierror.Classify(fmt.Errorf("hook: %w", err))
	if !apierror.IsCompactable(classified) || classified.Retryable {
		t.Fatalf("expected compactable, not blind retry: %#v", classified)
	}
}

type guardTestProvider struct {
	model.Provider
	id string
}

func (p *guardTestProvider) Model() string { return p.id }

func TestContextGuardLiveModelAndConfiguredBudget(t *testing.T) {
	provider := &guardTestProvider{id: "gpt-4o"}
	guard := newContextGuardHook("gpt-4o", 1000, contextGuardOptions{ContextLimit: 128000})
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: strings.Repeat("token ", 9000)}}}
	evt := &hooks.Event{Type: hooks.EventModelCallBefore, Input: req, Metadata: map[string]any{"provider": provider}}
	if err := guard.Before(context.Background(), evt); err != nil {
		t.Fatalf("large live model rejected request: %v", err)
	}
	provider.id = "gpt-4"
	var overflow *ContextBudgetOverflowError
	if err := guard.Before(context.Background(), evt); !errors.As(err, &overflow) || overflow.ModelID != "gpt-4" || overflow.ContextLimit != 8192 {
		t.Fatalf("switched provider not used: %v", err)
	}
	provider.id = "custom-deployment"
	if err := guard.Before(context.Background(), evt); err != nil {
		t.Fatalf("switched custom provider should use configured window: %v", err)
	}
	provider.id = "gpt-4"
	req.Model = "gpt-4o"
	if err := guard.Before(context.Background(), evt); err != nil {
		t.Fatalf("explicit request model must take precedence: %v", err)
	}
	req.Model = "custom-deployment"
	guard = newContextGuardHook("gpt-4", 0, contextGuardOptions{ContextLimit: 32000})
	if err := guard.Before(context.Background(), evt); err != nil {
		t.Fatalf("configured custom window ignored: %v", err)
	}
	req.Model = "gpt-4o"
	guard = newContextGuardHook("gpt-4o", 0, contextGuardOptions{ContextLimit: 1024})
	if err := guard.Before(context.Background(), evt); !errors.As(err, &overflow) || overflow.ContextLimit != 1024 {
		t.Fatalf("configured effective cap must apply to known models too: %v", err)
	}
}

func TestContextGuardEffectiveKnownAndUnknownLimits(t *testing.T) {
	tests := []struct {
		model      string
		configured int
		want       int
	}{
		{"gpt-4", 128000, 8192},
		{"gpt-4", 4096, 4096},
		{"gpt-4", 8192, 8192},
		{"gpt-4", 0, 8192},
		{"gpt-5-mini", 0, 400000},
		{"gpt-5-nano", 128000, 128000},
		{"custom-deployment", 128000, 128000},
		{"custom-deployment", 4096, 4096},
		{"custom-deployment", 0, 8192},
		{"custom-deployment", -1, 8192},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/configured=%d", tt.model, tt.configured), func(t *testing.T) {
			guard := newContextGuardHook("gpt-4o", 0, contextGuardOptions{ContextLimit: tt.configured})
			// Force a typed overflow so its reported effective context window can
			// be checked independently of tokenizer estimates and trimming.
			req := &model.ChatRequest{Model: tt.model, MaxTokens: 2000000, Messages: []model.Message{{Role: model.RoleUser, Content: "task"}}}
			var overflow *ContextBudgetOverflowError
			err := guard.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req})
			if !errors.As(err, &overflow) || overflow.ContextLimit != tt.want {
				t.Fatalf("effective context should be %d, got %#v (%v)", tt.want, overflow, err)
			}
		})
	}
}

func TestModelCatalogContextWindowsAgreeWithSDK(t *testing.T) {
	for _, deployment := range []string{"", "custom-deployment", "gpt-5-mini"} {
		t.Run(deployment, func(t *testing.T) {
			t.Setenv("AZURE_OPENAI_DEPLOYMENT", deployment)
			foundDeployment := deployment == ""
			for _, info := range modelinfo.All() {
				if info.Provider == "azure" && info.Model == deployment {
					foundDeployment = true
				}
				if info.Model == "custom-deployment" {
					if _, known := model.KnownContextLimit(info.Model); known || info.ContextWindow != 0 {
						t.Fatalf("custom deployment must remain explicitly unknown: %+v", info)
					}
					continue
				}
				want, known := model.KnownContextLimit(info.Model)
				if !known {
					t.Errorf("catalog model %s/%s is missing from SDK capabilities", info.Provider, info.Model)
				}
				if info.ContextWindow != want {
					t.Errorf("%s/%s UI window=%d, SDK=%d", info.Provider, info.Model, info.ContextWindow, want)
				}
				if got, ok := modelinfo.Lookup(info.Provider, info.Model); ok && got != info {
					t.Errorf("Lookup disagrees with catalog: %+v != %+v", got, info)
				}
				if got, ok := modelinfo.LookupByModel(info.Model); ok && got.ContextWindow != want {
					t.Errorf("LookupByModel window=%d, SDK=%d", got.ContextWindow, want)
				}
			}
			if !foundDeployment {
				t.Fatal("environment deployment missing from catalog")
			}
		})
	}
	if _, ok := modelinfo.Lookup("openai", "gpt-4"); ok {
		t.Fatal("SDK capability lookup must not expand provider catalog")
	}
	if _, ok := modelinfo.LookupByModel("custom-deployment"); ok {
		t.Fatal("unknown deployment must not acquire a provider")
	}
}

func TestContextGuardCountsCurrentSchemas(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 1, contextGuardOptions{ContextLimit: 1024})
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: "keep my task"}}}
	evt := &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}
	if err := guard.Before(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	schema := map[string]any{"type": "object", "description": strings.Repeat("parameter ", 2000)}
	req.Tools = []model.ToolDefinition{{Type: "function", Function: model.FunctionDef{Name: "late_tool", Parameters: schema}}}
	var overflow *ContextBudgetOverflowError
	if err := guard.Before(context.Background(), evt); !errors.As(err, &overflow) || overflow.SchemaTokens < 1024 || overflow.BudgetTokens >= 0 {
		t.Fatalf("large actual tool schema should exhaust budget: %v", err)
	}
	req.Tools = nil
	req.ResponseFormat = "json_schema"
	req.Metadata = map[string]any{"json_schema": schema}
	if err := guard.Before(context.Background(), evt); !errors.As(err, &overflow) || overflow.SchemaTokens < 1024 {
		t.Fatalf("output schema was not counted: %v", err)
	}
	req.Metadata = nil
	if err := guard.Before(context.Background(), evt); err != nil {
		t.Fatalf("removed schemas should free budget: %v", err)
	}
}

func TestContextGuardOutputReserve(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0, contextGuardOptions{ContextLimit: 1000, MaxOutputTokens: 900})
	req := &model.ChatRequest{Messages: []model.Message{{Role: model.RoleUser, Content: strings.Repeat("token ", 200)}}}
	evt := &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}
	var overflow *ContextBudgetOverflowError
	if err := guard.Before(context.Background(), evt); !errors.As(err, &overflow) || overflow.OutputTokens != 900 {
		t.Fatalf("configured output reserve ignored: %v", err)
	}
	req.MaxTokens = 200
	if err := guard.Before(context.Background(), evt); err != nil {
		t.Fatalf("request output limit should override configured default: %v", err)
	}
	req.MaxTokens = 1200
	if err := guard.Before(context.Background(), evt); !errors.As(err, &overflow) || overflow.BudgetTokens != -200 {
		t.Fatalf("oversized output reservation should reject: %v", err)
	}
}

func TestContextGuardShrinksToolsBeforeDroppingHistory(t *testing.T) {
	guard := newContextGuardHook("gpt-4", 0, contextGuardOptions{ContextLimit: 2048})
	content, _ := json.Marshal(map[string]any{"preview": strings.Repeat("界", 10000), "storage_key": "stored-original", "full_size_bytes": 90000})
	req := &model.ChatRequest{Messages: []model.Message{
		{Role: model.RoleSystem, Content: "system"},
		{Role: model.RoleUser, Content: "earlier task"},
		{Role: model.RoleAssistant, Content: "earlier answer"},
		{Role: model.RoleUser, Content: "current task"},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "one", Name: "read", Arguments: "{}"}, {ID: "two", Name: "read", Arguments: "{}"}}},
		{Role: model.RoleTool, ToolCallID: "one", Content: string(content)},
		{Role: model.RoleTool, ToolCallID: "two", Content: "small result"},
	}}
	original := append([]model.Message(nil), req.Messages...)
	if err := guard.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != len(original) {
		t.Fatal("history dropped before payload reduction")
	}
	for i, m := range req.Messages {
		if i != 5 && !reflect.DeepEqual(m, original[i]) {
			t.Fatalf("message %d unexpectedly changed", i)
		}
	}
	if original[5].Content != string(content) {
		t.Fatal("shared input history was mutated")
	}
	var reduced map[string]any
	if err := json.Unmarshal([]byte(req.Messages[5].Content), &reduced); err != nil || !utf8.ValidString(req.Messages[5].Content) {
		t.Fatalf("invalid reduced content: %v", err)
	}
	if reduced["storage_key"] != "stored-original" || reduced["full_size_bytes"] != float64(90000) {
		t.Fatalf("lost original artifact metadata: %#v", reduced)
	}
}

func TestContextGuardOverflowPreservesTaskAndProgress(t *testing.T) {
	for _, messages := range [][]model.Message{
		{{Role: model.RoleSystem, Content: strings.Repeat("token ", 1000)}},
		{{Role: model.RoleUser, Content: "task", Parts: []model.ContentPart{{Type: "text", Text: strings.Repeat("attachment ", 1000)}}}},
		{{Role: model.RoleUser, Content: "task"}, {Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "done", Name: "write", Arguments: strings.Repeat("arg ", 1000)}}}, {Role: model.RoleTool, ToolCallID: "done", Content: "mutation completed"}},
	} {
		guard := newContextGuardHook("gpt-4", 0, contextGuardOptions{ContextLimit: 256})
		req := &model.ChatRequest{Messages: messages}
		original := append([]model.Message(nil), messages...)
		var overflow *ContextBudgetOverflowError
		if err := guard.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); !errors.As(err, &overflow) {
			t.Fatalf("expected typed overflow, got %v", err)
		}
		if !reflect.DeepEqual(req.Messages, original) {
			t.Fatal("failed preflight mutated user input or tool progress")
		}
	}
}

func TestCapResultUniversalAndUTF8Safe(t *testing.T) {
	large := strings.Repeat("界🙂\"\n", maxToolResultBytes/4)
	object := struct {
		Content string `json:"content"`
	}{large}
	tests := map[string]any{
		"string": large, "bytes": []byte(large), "slice": []string{large},
		"array": [1]string{large}, "struct": object, "pointer": &object,
		"typed map": map[string]string{"content": large}, "map": map[string]any{"content": large},
		"raw json":     json.RawMessage(`{"content":` + fmt.Sprintf("%q", strings.Repeat("界", maxToolResultBytes)) + `}`),
		"invalid utf8": strings.Repeat("x\xff", maxToolResultBytes),
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			result := capResult(value)
			data, err := json.Marshal(result)
			if err != nil || len(data) > maxToolResultBytes || !utf8.Valid(data) {
				t.Fatalf("invalid capped encoding: bytes=%d err=%v", len(data), err)
			}
			envelope, ok := result.(map[string]any)
			if !ok || envelope["truncated"] != true {
				t.Fatalf("expected structured truncation, got %T", result)
			}
			preview, ok := envelope["preview"].(string)
			if !ok || !utf8.ValidString(preview) || envelope["shown_bytes"] != len(preview) {
				t.Fatal("invalid preview or shown-byte accounting")
			}
			if _, exists := envelope["storage_key"]; exists {
				t.Fatal("invented artifact reference")
			}
		})
	}
}

func TestCapResultPreservesReferencesAndSmallValues(t *testing.T) {
	for _, value := range []any{nil, 42, true, "small", []string{"small"}, map[string]any{"small": true}} {
		if got := capResult(value); !reflect.DeepEqual(got, value) {
			t.Fatalf("small value changed: %#v -> %#v", value, got)
		}
	}
	value := map[string]any{"preview": strings.Repeat("x", maxToolResultBytes*2), "storage_key": "original-key", "full_size_bytes": 999999, "artifact_uri": "file:///original"}
	result := capResult(value).(map[string]any)
	if result["storage_key"] != value["storage_key"] || result["artifact_uri"] != value["artifact_uri"] || result["full_size_bytes"] != float64(999999) {
		t.Fatal("lost full artifact reference")
	}
	encoded, _ := json.Marshal(value)
	if result := capResult(string(encoded)).(map[string]any); result["storage_key"] != "original-key" {
		t.Fatal("lost reference in a JSON string result")
	}
	for _, value := range []any{make(chan int), func() {}, map[string]any{"bad": make(chan int)}} {
		result := capResult(value)
		if data, err := json.Marshal(result); err != nil || len(data) > maxToolResultBytes {
			t.Fatalf("unserializable value bypassed cap: %v", err)
		}
	}
	if got := capResult("bad\xff").(string); !utf8.ValidString(got) {
		t.Fatal("small invalid UTF8 was not normalized")
	}
}
