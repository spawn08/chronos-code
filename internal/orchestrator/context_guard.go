package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/tokencache"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
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

	// trims holds the last trimmed history per conversation (session and
	// invocation, see contextTrimKey) so later calls send
	// the same prefix. Providers with prompt caching (Anthropic) reuse a
	// cached prompt only when everything before the new messages is
	// byte-identical; recomputing the trim from the full history on every
	// call changes old messages each time and rewrites the whole cache.
	mu    sync.Mutex
	trims map[string]*contextTrim
	clock uint64
	// scale maps a model ID to provider-reported prompt tokens per locally
	// counted token. The local counter (o200k) undercounts newer Claude and
	// other non-OpenAI tokenizers by 20-30%, so the uncalibrated budget would
	// let requests run past the configured ceiling.
	scale map[string]float64
}

// contextTrim is a trimmed copy of the first baseLen request messages. A later
// request whose first baseLen messages hash to base reuses trimmed and appends
// only its newer messages.
type contextTrim struct {
	used    uint64 // recency stamp for eviction
	modelID string
	baseLen int
	base    [sha256.Size]byte
	trimmed []model.Message
	dropped []ContextSourceKind
}

// contextGuardHeadroom is the share of the budget left free after a trim, so
// the trimmed prefix can be reused for many calls before it must be rebuilt.
const contextGuardHeadroom = 0.2

// Calibration bounds and smoothing: one odd response (an unusual attachment)
// must not swing the budget. Calibration only ever shrinks the budget:
// providers differ in what their prompt count covers (Ollama and llama.cpp
// omit KV-cached tokens, for example), and an under-report must never let a
// request grow past the model's window.
const (
	minTokenScale       = 1.0
	maxTokenScale       = 2.0
	tokenScaleWeight    = 0.3
	minCalibrationCount = 4000
)

// guardEstimateMetadataKey carries the request's local token estimate from
// Before to After for calibration.
const guardEstimateMetadataKey = "chronos_code.context_guard_estimate"

type guardEstimate struct {
	modelID string
	tokens  int
}

// maxContextTrims bounds remembered conversations; the least recently used
// is forgotten first (finished subagent runs), which costs only a rebuild.
const maxContextTrims = 256

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
	// A deployment can serve a smaller window than the catalog (a local
	// Ollama server's context setting), and may truncate silently past it.
	if provider, ok := evt.Metadata["provider"].(model.Provider); ok && provider != nil {
		if served, ok := model.ServedContextLimit(ctx, provider); ok && served < contextLimit {
			contextLimit = served
		}
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
	// The ceiling is in provider tokens; convert it to local counter units.
	effectiveLimit := int(float64(contextLimit-outputTokens)/h.tokenScale(modelID)) - schemaTokens
	total := counter.CountTokens(req.Messages)
	if total <= effectiveLimit {
		evt.Metadata[guardEstimateMetadataKey] = guardEstimate{modelID: modelID, tokens: total + schemaTokens}
		return nil
	}

	// Count system/pinned messages that should not be trimmed.
	protectedPrefix := 0
	for protectedPrefix < len(req.Messages) && req.Messages[protectedPrefix].Role == model.RoleSystem {
		protectedPrefix++
	}
	trimKey := contextTrimKey(ctx)
	if trimmed, dropped, ok := h.reuseTrim(trimKey, modelID, effectiveLimit, counter, req.Messages); ok {
		for _, source := range dropped {
			contextSourceOmitted(ctx, source, ContextOmittedBudget)
		}
		req.Messages = trimmed
		evt.Metadata[guardEstimateMetadataKey] = guardEstimate{modelID: modelID, tokens: counter.CountTokens(trimmed) + schemaTokens}
		return nil
	}
	// Trim below the budget so the next calls fit with the same prefix.
	target := effectiveLimit - int(float64(effectiveLimit)*contextGuardHeadroom)
	trimmed, droppedSources := trimMessages(counter, req.Messages, protectedPrefix, target)
	if model.AnthropicThinkingBoundToConversation(modelID) {
		// Trimming edits earlier turns, so thinking kept from before it no
		// longer verifies. Later blocks bind to this remembered, append-only
		// history and stay valid.
		trimmed = model.StripAnthropicThinking(trimmed)
	}
	for _, source := range droppedSources {
		contextSourceOmitted(ctx, source, ContextOmittedBudget)
	}
	if tokens := counter.CountTokens(trimmed); tokens > effectiveLimit {
		return &ContextBudgetOverflowError{ModelID: modelID, MessageTokens: tokens,
			BudgetTokens: effectiveLimit, ContextLimit: contextLimit,
			SchemaTokens: schemaTokens, OutputTokens: outputTokens}
	}
	h.rememberTrim(trimKey, modelID, req.Messages, trimmed, droppedSources)
	req.Messages = trimmed
	evt.Metadata[guardEstimateMetadataKey] = guardEstimate{modelID: modelID, tokens: counter.CountTokens(trimmed) + schemaTokens}
	return nil
}

// tokenScale returns the calibrated provider/local token ratio for modelID,
// or 1 before any usage has been observed.
func (h *contextGuardHook) tokenScale(modelID string) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if scale, ok := h.scale[modelID]; ok {
		return scale
	}
	return 1
}

// calibrate folds one call's provider-reported prompt size into modelID's
// scale. Small requests are skipped: fixed provider framing dominates them.
func (h *contextGuardHook) calibrate(modelID string, estimated, actual int) {
	if estimated < minCalibrationCount || actual <= 0 {
		return
	}
	observed := min(max(float64(actual)/float64(estimated), minTokenScale), maxTokenScale)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scale == nil {
		h.scale = make(map[string]float64)
	}
	previous, ok := h.scale[modelID]
	if !ok {
		previous = 1
	}
	h.scale[modelID] = previous + tokenScaleWeight*(observed-previous)
}

// contextTrimKey identifies one conversation. Every top-level run (each user
// turn) gets a fresh invocation, so a top-level agent is keyed by session and
// role to keep its trim across turns. Subagents inherit the parent's session
// but are separate conversations, so a child run is keyed by its invocation;
// keying by session alone would let concurrent agents evict each other.
func contextTrimKey(ctx context.Context) string {
	identity, _ := agent.RunIdentityFromContext(ctx)
	session := storage.SessionFromContext(ctx)
	if identity.ParentInvocationID != "" {
		return session + "\x00child\x00" + identity.InvocationID
	}
	return session + "\x00role\x00" + identity.RoleID
}

// reuseTrim returns the remembered trimmed prefix plus the request's newer
// messages when the request still starts with the remembered history and the
// result fits the budget.
func (h *contextGuardHook) reuseTrim(key, modelID string, limit int, counter model.TokenCounter, messages []model.Message) ([]model.Message, []ContextSourceKind, bool) {
	h.mu.Lock()
	trim := h.trims[key]
	if trim != nil {
		h.clock++
		trim.used = h.clock
	}
	h.mu.Unlock()
	if trim == nil || trim.modelID != modelID || stableLen(messages) < trim.baseLen {
		return nil, nil, false
	}
	base, ok := hashMessages(messages[:trim.baseLen])
	if !ok || base != trim.base {
		return nil, nil, false
	}
	candidate := make([]model.Message, 0, len(trim.trimmed)+len(messages)-trim.baseLen)
	candidate = append(candidate, trim.trimmed...)
	candidate = append(candidate, messages[trim.baseLen:]...)
	if counter.CountTokens(candidate) > limit {
		return nil, nil, false
	}
	return candidate, trim.dropped, true
}

// rememberTrim stores trimmed as the reusable form of original. Trailing
// Uncached messages change on every call, so they are excluded from both.
func (h *contextGuardHook) rememberTrim(key, modelID string, original, trimmed []model.Message, dropped []ContextSourceKind) {
	baseLen, trimmedLen := stableLen(original), stableLen(trimmed)
	if len(original)-baseLen != len(trimmed)-trimmedLen {
		return
	}
	base, ok := hashMessages(original[:baseLen])
	if !ok {
		return
	}
	trim := &contextTrim{modelID: modelID, baseLen: baseLen, base: base,
		trimmed: append([]model.Message(nil), trimmed[:trimmedLen]...),
		dropped: append([]ContextSourceKind(nil), dropped...)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.trims == nil {
		h.trims = make(map[string]*contextTrim)
	}
	if _, exists := h.trims[key]; !exists && len(h.trims) >= maxContextTrims {
		oldest := ""
		for k, t := range h.trims {
			if oldest == "" || t.used < h.trims[oldest].used {
				oldest = k
			}
		}
		delete(h.trims, oldest)
	}
	h.clock++
	trim.used = h.clock
	h.trims[key] = trim
}

// stableLen is the number of messages before any trailing Uncached ones.
func stableLen(messages []model.Message) int {
	n := len(messages)
	for n > 0 && messages[n-1].Uncached {
		n--
	}
	return n
}

// hashMessages fingerprints the serialized messages (provider-opaque state
// is excluded by the model's JSON tags).
func hashMessages(messages []model.Message) ([sha256.Size]byte, bool) {
	var sum [sha256.Size]byte
	digest := sha256.New()
	if err := json.NewEncoder(digest).Encode(messages); err != nil {
		return sum, false
	}
	copy(sum[:], digest.Sum(nil))
	return sum, true
}

func (h *contextGuardHook) After(_ context.Context, evt *hooks.Event) error {
	if evt == nil || evt.Type != hooks.EventModelCallAfter || evt.Error != nil {
		return nil
	}
	estimate, ok := evt.Metadata[guardEstimateMetadataKey].(guardEstimate)
	if !ok {
		return nil
	}
	delete(evt.Metadata, guardEstimateMetadataKey)
	if resp, ok := evt.Output.(*model.ChatResponse); ok && resp != nil && resp.UsageKnown {
		h.calibrate(estimate.modelID, estimate.tokens, resp.Usage.PromptWindowTokens())
	}
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
	dropSources := func(sources []ContextSourceKind) {
		for _, source := range sources {
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
	}
	dropSources(droppableContextSources)
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
	// Skills and project instructions sit in the provider's system prompt:
	// dropping one invalidates the cached prompt after it and loses standing
	// guidance, so they go only after every older turn is gone.
	before := len(msgs)
	dropSources(lastResortContextSources)
	if len(msgs) != before {
		// Room freed by dropping standing guidance must not be refilled with
		// old prompts: that would trade the guidance for older turns.
		droppedUserPrompts = nil
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
	ContextSourceRepositoryContext,
	ContextSourceSessionSummaries,
	ContextSourceMemory,
	ContextSourceLearnedPattern,
	ContextSourceUserHook,
}

// lastResortContextSources are droppable request-time context that the guard
// removes only when trimming old turns was not enough.
var lastResortContextSources = []ContextSourceKind{
	ContextSourceSkills,
	ContextSourceProjectDocs,
}

func contextSourceFromMessage(message model.Message) ContextSourceKind {
	for _, candidate := range []struct {
		prefix string
		source ContextSourceKind
	}{
		{"Fresh LSP diagnostics for referenced files", ContextSourceDiagnostics},
		{"[Pre-loaded context]", ContextSourceGraphPrediction},
		{"[Repository context]", ContextSourceRepositoryContext},
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
