package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
)

func TestContextGuardTrimsMessagesOverLimit(t *testing.T) {
	guard := newContextGuardHook("claude-haiku-4-5", 10)

	// claude-haiku-4-5 has a 200k context limit. Build a request that exceeds
	// the effective limit (200k * 0.85 - tool reserve).
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
			{Role: model.RoleAssistant, Content: largeContent},
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
	// When the user message before the tool-call assistant is dropped, the
	// tool-call assistant becomes the new first non-protected message and is
	// dropped next. The tool result should be dropped as an orphan with it.
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
}
