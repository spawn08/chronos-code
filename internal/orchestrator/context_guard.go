package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
)

// contextGuardHook prevents token-explosion failures by trimming the request
// messages before every model call when they exceed the model's context limit.
// Unlike the SDK's enforceContextBudget (which runs once at session start),
// this guard fires on every model call — including follow-up rounds in the
// tool-calling loop where large tool results accumulate without any budget
// check.
type contextGuardHook struct {
	modelID string
	// reserveForTools is a fixed token budget reserved for tool definitions so
	// the message trim accounts for their invisible overhead.
	reserveForTools int
}

const (
	// defaultToolReserveTokens is a conservative per-tool overhead estimate.
	// Each tool definition consumes name + description + JSON schema tokens.
	defaultToolReserveTokens = 150
	// contextGuardMargin is the fraction of the context window to keep free
	// for the model's output and overhead.
	contextGuardMargin = 0.15
)

func newContextGuardHook(modelID string, numTools int) *contextGuardHook {
	return &contextGuardHook{
		modelID:         modelID,
		reserveForTools: numTools * defaultToolReserveTokens,
	}
}

func (h *contextGuardHook) Before(_ context.Context, evt *hooks.Event) error {
	if evt.Type != hooks.EventModelCallBefore {
		return nil
	}
	req, ok := evt.Input.(*model.ChatRequest)
	if !ok || req == nil || len(req.Messages) == 0 {
		return nil
	}

	modelID := req.Model
	if modelID == "" {
		modelID = h.modelID
	}
	contextLimit := model.ContextLimit(modelID, 0)
	if contextLimit <= 0 {
		return nil
	}

	toolTokens := h.reserveForTools
	if toolTokens == 0 && len(req.Tools) > 0 {
		toolTokens = estimateToolTokens(req.Tools)
	}

	effectiveLimit := int(float64(contextLimit) * (1.0 - contextGuardMargin))
	effectiveLimit -= toolTokens
	if effectiveLimit <= 0 {
		effectiveLimit = contextLimit / 2
	}

	counter := model.NewTokenCounter(modelID)
	total := counter.CountTokens(req.Messages)
	if total <= effectiveLimit {
		return nil
	}

	// Count system/pinned messages that should not be trimmed.
	protectedPrefix := 0
	for protectedPrefix < len(req.Messages) && req.Messages[protectedPrefix].Role == model.RoleSystem {
		protectedPrefix++
	}
	if protectedPrefix >= len(req.Messages) {
		return nil
	}

	// Trim oldest conversation messages until we fit. Always keep at least
	// one user/assistant turn: Anthropic and Gemini strip system messages
	// into a separate field, so a system-only request 400s with
	// "it must contain at least one message" — which is exactly the failure
	// seen after a tool-calling round when orphan tool-results wipe the tail.
	trimmed := trimMessages(counter, req.Messages, protectedPrefix, effectiveLimit)
	if tokens := counter.CountTokens(trimmed); tokens > effectiveLimit {
		return fmt.Errorf("context guard: messages (%d tokens) exceed safe model budget (%d tokens after tool/output reserve) even after trimming; use /clear to start a fresh session",
			tokens, effectiveLimit)
	}
	req.Messages = trimmed
	return nil
}

func (h *contextGuardHook) After(_ context.Context, _ *hooks.Event) error {
	return nil
}

// trimMessages drops the oldest non-protected messages until total tokens <=
// limit. When a dropped assistant message carries tool calls, any immediately
// following tool-result messages are dropped with it.
func trimMessages(counter model.TokenCounter, messages []model.Message, protectedPrefix, limit int) []model.Message {
	if limit <= 0 || counter == nil || protectedPrefix < 0 {
		return messages
	}
	if protectedPrefix > len(messages) {
		protectedPrefix = len(messages)
	}
	// Work on a copy so we don't mutate the original slice.
	msgs := make([]model.Message, len(messages))
	copy(msgs, messages)

	total := counter.CountTokens(msgs)
	if total <= limit {
		return msgs
	}
	base := counter.CountTokens(nil)
	cost := func(m model.Message) int { return counter.CountTokens([]model.Message{m}) - base }

	original := msgs
	for total > limit && len(msgs) > protectedPrefix+1 {
		total -= cost(msgs[protectedPrefix])
		msgs = append(msgs[:protectedPrefix:protectedPrefix], msgs[protectedPrefix+1:]...)
		// Drop orphaned tool results, but never the last remaining
		// conversation message — the inner loop used to wipe the whole
		// tail after the assistant/tool-call turn was dropped.
		for len(msgs) > protectedPrefix+1 && msgs[protectedPrefix].Role == model.RoleTool {
			total -= cost(msgs[protectedPrefix])
			msgs = append(msgs[:protectedPrefix:protectedPrefix], msgs[protectedPrefix+1:]...)
		}
	}
	if !hasUserOrAssistant(msgs, protectedPrefix) {
		msgs = restoreLastUserTurn(original, protectedPrefix)
	}
	if counter.CountTokens(msgs) > limit {
		if collapsed := collapseToLastUser(original, protectedPrefix); len(collapsed) > 0 {
			msgs = collapsed
		}
	}
	return msgs
}

func hasUserOrAssistant(messages []model.Message, from int) bool {
	for i := from; i < len(messages); i++ {
		switch messages[i].Role {
		case model.RoleUser, model.RoleAssistant:
			return true
		}
	}
	return false
}

// restoreLastUserTurn keeps the protected prefix plus the most recent user
// message and everything after it (the in-flight tool-calling turn).
func restoreLastUserTurn(messages []model.Message, protectedPrefix int) []model.Message {
	if protectedPrefix < 0 {
		protectedPrefix = 0
	}
	if protectedPrefix > len(messages) {
		protectedPrefix = len(messages)
	}
	lastUser := -1
	for i := len(messages) - 1; i >= protectedPrefix; i-- {
		if messages[i].Role == model.RoleUser {
			lastUser = i
			break
		}
	}
	out := make([]model.Message, 0, len(messages))
	out = append(out, messages[:protectedPrefix]...)
	if lastUser >= 0 {
		return append(out, messages[lastUser:]...)
	}
	if protectedPrefix < len(messages) {
		return append(out, messages[len(messages)-1])
	}
	return out
}

func collapseToLastUser(messages []model.Message, protectedPrefix int) []model.Message {
	if protectedPrefix < 0 {
		protectedPrefix = 0
	}
	if protectedPrefix > len(messages) {
		protectedPrefix = len(messages)
	}
	lastUser := -1
	for i := len(messages) - 1; i >= protectedPrefix; i-- {
		if messages[i].Role == model.RoleUser {
			lastUser = i
			break
		}
	}
	if lastUser < 0 {
		return nil
	}
	out := make([]model.Message, 0, protectedPrefix+1)
	out = append(out, messages[:protectedPrefix]...)
	return append(out, messages[lastUser])
}

// maxToolResultBytes is the hard cap on any single tool result. Results
// exceeding this are truncated before they enter the message history.
// This is a last-resort safety net — toolcompress should compress before
// this limit is reached, but some paths (MCP tools, incctx injections)
// can bypass compression.
const maxToolResultBytes = 100 << 10 // 100 KB

// wrapToolResultCap installs a size-limiting wrapper on every tool so that
// no single result can inject more than maxToolResultBytes into the
// conversation. This runs after toolcompress (which evicts large results to
// storage) as a safety net for results that bypass compression.
func wrapToolResultCap(a *agent.Agent) {
	for _, def := range a.Tools.List() {
		if def.Handler == nil {
			continue
		}
		orig := def.Handler
		wrapped := *def
		wrapped.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			result, err := orig(ctx, args)
			if err != nil || result == nil {
				return result, err
			}
			return capResult(result), nil
		}
		a.Tools.Register(&wrapped)
	}
}

func capResult(result any) any {
	switch v := result.(type) {
	case string:
		if len(v) > maxToolResultBytes {
			return v[:maxToolResultBytes] + fmt.Sprintf("\n... [truncated: %d bytes total, showing first %d]", len(v), maxToolResultBytes)
		}
	case map[string]any:
		data, err := json.Marshal(v)
		if err != nil || len(data) <= maxToolResultBytes {
			return result
		}
		return map[string]any{
			"truncated":   true,
			"preview":     string(data[:maxToolResultBytes]),
			"total_bytes": len(data),
			"shown_bytes": maxToolResultBytes,
		}
	}
	return result
}

func estimateToolTokens(tools []model.ToolDefinition) int {
	total := 0
	for _, t := range tools {
		total += 10 // name + type framing
		total += len(t.Function.Name) / 4
		total += len(t.Function.Description) / 4
		if t.Function.Parameters != nil {
			data, _ := json.Marshal(t.Function.Parameters)
			total += len(data) / 4
		}
	}
	return total
}
