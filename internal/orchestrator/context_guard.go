package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/tokencache"
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
	options contextGuardOptions
	tokens  tokencache.Cache
}

// contextGuardOptions accepts a configured context ceiling, clamped to the live
// model's known SDK window. Unknown deployments use the configured value;
// zero uses the SDK's known window or its default for unknown models.
// MaxOutputTokens is used only when the request does not specify MaxTokens.
type contextGuardOptions struct {
	ContextLimit    int
	MaxOutputTokens int
}

const contextGuardMargin = 0.15

// The legacy tool count is intentionally ignored: definitions are measured on
// every call, including tools registered after hook construction.
func newContextGuardHook(modelID string, _ int, options ...contextGuardOptions) *contextGuardHook {
	h := &contextGuardHook{modelID: modelID}
	if len(options) > 0 {
		h.options = options[0]
	}
	return h
}

// ContextBudgetOverflowError means preflight could not fit a request without
// deleting the current task or tool progress. Recovery must change the budget
// or input; retrying the same request cannot help.
type ContextBudgetOverflowError struct {
	ModelID       string
	MessageTokens int
	BudgetTokens  int
	ContextLimit  int
	SchemaTokens  int
	OutputTokens  int
}

func (e *ContextBudgetOverflowError) Error() string {
	return fmt.Sprintf("context guard: messages (%d tokens) exceed safe model budget (%d tokens after schema/output reserve) for %s; compact history or reduce attachments", e.MessageTokens, e.BudgetTokens, e.ModelID)
}

func (e *ContextBudgetOverflowError) ContextBudgetExceeded() bool { return true }

func (h *contextGuardHook) Before(ctx context.Context, evt *hooks.Event) error {
	if evt == nil || evt.Type != hooks.EventModelCallBefore {
		return nil
	}
	req, ok := evt.Input.(*model.ChatRequest)
	if !ok || req == nil {
		return nil
	}

	modelID := req.Model
	if provider, ok := evt.Metadata["provider"].(model.Provider); modelID == "" && ok && provider != nil {
		modelID = provider.Model()
	}
	if modelID == "" {
		modelID = h.modelID
	}
	contextLimit := h.options.ContextLimit
	if knownLimit, known := model.KnownContextLimit(modelID); known {
		if contextLimit <= 0 || knownLimit < contextLimit {
			contextLimit = knownLimit
		}
	} else if contextLimit <= 0 {
		contextLimit = model.ContextLimit(modelID, 0)
	}

	baseCounter := h.tokens.ForModel(modelID)
	if evt.Metadata == nil {
		evt.Metadata = make(map[string]any)
	}
	evt.Metadata[tokenCounterMetadataKey] = requestTokenCounter{modelID: modelID, counter: baseCounter}
	counter := guardTokenCounter{baseCounter}
	schemaTokens := 0
	if len(req.Tools) > 0 {
		data, err := json.Marshal(req.Tools)
		if err != nil {
			return fmt.Errorf("context guard: encode tool schemas: %w", err)
		}
		schemaTokens += counter.CountString(string(data))
	}
	if req.ResponseFormat == "json_schema" && req.Metadata["json_schema"] != nil {
		data, err := json.Marshal(req.Metadata["json_schema"])
		if err != nil {
			return fmt.Errorf("context guard: encode output schema: %w", err)
		}
		schemaTokens += counter.CountString(string(data))
	}

	outputTokens := req.MaxTokens
	if outputTokens <= 0 {
		outputTokens = h.options.MaxOutputTokens
	}
	// Keep at least the existing safety margin for provider framing/tokenizer
	// differences, but never under-reserve an explicit output allowance.
	outputTokens = max(outputTokens, contextLimit-int(float64(contextLimit)*(1-contextGuardMargin)))
	effectiveLimit := contextLimit - outputTokens - schemaTokens
	total := counter.CountTokens(req.Messages)
	if total <= effectiveLimit {
		return nil
	}

	// Count system/pinned messages that should not be trimmed.
	protectedPrefix := 0
	for protectedPrefix < len(req.Messages) && req.Messages[protectedPrefix].Role == model.RoleSystem {
		protectedPrefix++
	}
	trimmed, droppedSources := trimMessages(counter, req.Messages, protectedPrefix, effectiveLimit)
	for _, source := range droppedSources {
		contextSourceOmitted(ctx, source, ContextOmittedBudget)
	}
	if tokens := counter.CountTokens(trimmed); tokens > effectiveLimit {
		return &ContextBudgetOverflowError{ModelID: modelID, MessageTokens: tokens,
			BudgetTokens: effectiveLimit, ContextLimit: contextLimit,
			SchemaTokens: schemaTokens, OutputTokens: outputTokens}
	}
	req.Messages = trimmed
	return nil
}

func (h *contextGuardHook) After(_ context.Context, _ *hooks.Event) error {
	return nil
}

const tokenCounterMetadataKey = "chronos_code.token_counter"

type requestTokenCounter struct {
	modelID string
	counter model.TokenCounter
}

// Reuse preflight counts in both budget hooks, while counting the actual current
// request (including any trimming or changes made by intervening hooks).
func tokenCounterForEvent(evt *hooks.Event, modelID string) model.TokenCounter {
	if cached, ok := evt.Metadata[tokenCounterMetadataKey].(requestTokenCounter); ok && cached.modelID == modelID {
		return cached.counter
	}
	return model.NewTokenCounter(modelID)
}

// guardTokenCounter includes attachment text/data and call IDs omitted by the
// SDK counter. Binary media is conservatively estimated by encoded byte size;
// exact provider-specific media accounting still belongs in the SDK.
type guardTokenCounter struct{ model.TokenCounter }

func (c guardTokenCounter) CountTokens(messages []model.Message) int {
	total := c.TokenCounter.CountTokens(messages)
	for _, m := range messages {
		total += c.CountString(m.ToolCallID)
		for _, call := range m.ToolCalls {
			total += c.CountString(call.ID)
		}
		for _, part := range m.Parts {
			total += c.CountString(part.Text) + c.CountString(part.ImageURL) + c.CountString(part.FileName)
			total += (len(part.Data) + 2) / 3
		}
		for _, audio := range m.Audio {
			total += c.CountString(audio.Transcript) + (len(audio.Data)+2)/3
		}
	}
	return total
}

// trimMessages shrinks tool payloads first, then removes only complete older
// user turns. The latest user message and every subsequent call/result stay.
func trimMessages(counter model.TokenCounter, messages []model.Message, protectedPrefix, limit int) ([]model.Message, []ContextSourceKind) {
	if limit <= 0 || counter == nil || protectedPrefix < 0 {
		return messages, nil
	}
	if protectedPrefix > len(messages) {
		protectedPrefix = len(messages)
	}
	// Work on a copy so we don't mutate the original slice.
	msgs := make([]model.Message, len(messages))
	copy(msgs, messages)

	// Rebuild each preview from the original, avoiding nested truncation
	// envelopes and preserving original full-result references.
	total := counter.CountTokens(msgs)
	for capBytes := maxToolResultBytes; total > limit; capBytes = max(256, capBytes/2) {
		for i, m := range messages {
			if m.Role == model.RoleTool && len(m.Content) > capBytes {
				content := cappedToolContent(m.Content, capBytes)
				saving := counter.CountString(msgs[i].Content) - counter.CountString(content)
				if saving > 0 {
					msgs[i].Content = content
					total -= saving
				}
				if total <= limit {
					break
				}
			}
		}
		if capBytes <= 256 {
			break
		}
	}
	var dropped []ContextSourceKind
	for _, source := range droppableContextSources {
		for i := 0; total > limit && i < len(msgs); i++ {
			if msgs[i].Role != model.RoleSystem || contextSourceFromMessage(msgs[i]) != source {
				continue
			}
			total -= counter.CountTokens([]model.Message{msgs[i]})
			msgs = append(msgs[:i], msgs[i+1:]...)
			dropped = append(dropped, source)
			i--
		}
	}
	protectedPrefix = 0
	for protectedPrefix < len(msgs) && msgs[protectedPrefix].Role == model.RoleSystem {
		protectedPrefix++
	}
	// A brief follow-up often depends on the user's earlier request. Tool
	// output can exhaust the window and force whole turns out of the request;
	// retain a few small user prompts even when their assistant/tool turns go.
	keepUserPrompts := false
	for i := len(msgs) - 1; i >= protectedPrefix; i-- {
		if msgs[i].Role == model.RoleUser {
			keepUserPrompts = len(msgs[i].Content) <= 256
			break
		}
	}
	var droppedUserPrompts []model.Message
	for total > limit {
		nextUser := -1
		for i := protectedPrefix + 1; i < len(msgs); i++ {
			if msgs[i].Role == model.RoleUser {
				nextUser = i
				break
			}
		}
		if nextUser < 0 {
			break
		}
		if keepUserPrompts {
			for _, m := range msgs[protectedPrefix:nextUser] {
				if m.Role == model.RoleUser && len(m.Content) <= 4096 {
					droppedUserPrompts = append(droppedUserPrompts, m)
					if len(droppedUserPrompts) > 4 {
						droppedUserPrompts = droppedUserPrompts[1:]
					}
				}
			}
		}
		kept := append([]model.Message(nil), msgs[:protectedPrefix]...)
		for _, m := range msgs[protectedPrefix:nextUser] {
			if m.Role == model.RoleSystem {
				kept = append(kept, m)
			}
		}
		protectedPrefix = len(kept)
		msgs = append(kept, msgs[nextUser:]...)
		total = counter.CountTokens(msgs)
	}
	if total <= limit && len(droppedUserPrompts) > 0 {
		// Do not trade the live task or tool progress for an old prompt.
		for i := len(droppedUserPrompts) - 1; i >= 0 && i >= len(droppedUserPrompts)-4; i-- {
			prompt := droppedUserPrompts[i]
			if total+counter.CountTokens([]model.Message{prompt}) > limit {
				continue
			}
			msgs = append(msgs[:protectedPrefix], append([]model.Message{prompt}, msgs[protectedPrefix:]...)...)
			total = counter.CountTokens(msgs)
		}
	}
	return msgs, dropped
}

// These headers identify context generated by Chronos Code at request time.
// Only this best-effort context may be dropped under pressure; static prompts,
// the current user task, attachments, and tool-call progress remain intact.
var droppableContextSources = []ContextSourceKind{
	ContextSourceDiagnostics,
	ContextSourceGraphPrediction,
	ContextSourceSessionSummaries,
	ContextSourceMemory,
	ContextSourceLearnedPattern,
	ContextSourceSkills,
	ContextSourceUserHook,
	ContextSourceProjectDocs,
}

func contextSourceFromMessage(message model.Message) ContextSourceKind {
	for _, candidate := range []struct {
		prefix string
		source ContextSourceKind
	}{
		{"Fresh LSP diagnostics for referenced files", ContextSourceDiagnostics},
		{"[Pre-loaded context]", ContextSourceGraphPrediction},
		{"Relevant context from prior sessions:", ContextSourceSessionSummaries},
		{layerDataHeader, ContextSourceMemory},
		{"Known project/user/feedback notes", ContextSourceMemory},
		{"Learned pattern advisory only", ContextSourceLearnedPattern},
		{"<skill", ContextSourceSkills},
		{"<user_hook_context>", ContextSourceUserHook},
		{"<project_instructions", ContextSourceProjectDocs},
	} {
		if strings.HasPrefix(message.Content, candidate.prefix) {
			return candidate.source
		}
	}
	return ""
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

// maxToolResultBytes is the serialized size target for a tool result. Payloads
// exceeding this are truncated before they enter the message history; existing
// artifact references remain indivisible and subject to the request budget.
// This is a last-resort safety net — toolcompress should compress before
// this limit is reached, but some paths (MCP tools, incctx injections)
// can bypass compression.
const maxToolResultBytes = 100 << 10 // 100 KB

// wrapToolResultCap installs a size-limiting wrapper on every tool so that
// result payloads are bounded to maxToolResultBytes in the conversation.
// This runs after toolcompress (which evicts large results to
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
	if s, ok := result.(string); ok {
		result = strings.ToValidUTF8(s, "\uFFFD")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return map[string]any{"error": "tool result is not JSON serializable", "truncated": true}
	}
	if len(data) <= maxToolResultBytes {
		return result
	}
	if content, ok := result.(string); ok {
		data = []byte(content)
	}
	return cappedResult(data, maxToolResultBytes)
}

func cappedToolContent(content string, limit int) string {
	data, _ := json.Marshal(cappedResult([]byte(strings.ToValidUTF8(content, "\uFFFD")), limit))
	return string(data)
}

// cappedResult bounds the entire serialized envelope, including JSON escaping
// and metadata. It carries real reference fields forward, never inventing a
// storage key for content that has not actually been externalized.
func cappedResult(data []byte, limit int) map[string]any {
	out := map[string]any{"truncated": true, "total_bytes": len(data), "shown_bytes": 0, "preview": ""}
	var object map[string]any
	if json.Unmarshal(data, &object) == nil {
		for _, key := range []string{"storage_key", "artifact_id", "artifact_path", "artifact_uri", "full_content_ref", "full_size_bytes", "total_bytes"} {
			if value, ok := object[key]; ok {
				out[key] = value
			}
		}
	}
	// References are indivisible. If they alone exceed a request-time preview
	// target, keep them; the guard will reject the still-oversized request.
	preview := strings.ToValidUTF8(string(data), "\uFFFD")
	lo, hi := 0, min(len(preview), limit)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		out["preview"] = utf8Prefix(preview, mid)
		out["shown_bytes"] = len(out["preview"].(string))
		encoded, _ := json.Marshal(out)
		if len(encoded) <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	out["preview"] = utf8Prefix(preview, lo)
	out["shown_bytes"] = len(out["preview"].(string))
	return out
}

func utf8Prefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
