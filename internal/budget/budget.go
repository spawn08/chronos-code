// Package budget implements PRD P2-009: real-time per-session token/cost
// budget tracking with a progressive compression ramp. Tracker is a
// chronos hooks.Hook: wiring it into an agent's hook chain is the only
// integration point required — it observes every model call to accumulate
// usage (After) and blocks further calls once a session's budget is
// exhausted (Before), with no other package needing to know it exists.
package budget

import (
	"context"
	"fmt"
	"sync"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/storage"
)

// Level describes how aggressively tool-result compression should be
// tightened as a session's token usage approaches its budget.
type Level string

const (
	LevelNormal     Level = "normal"
	LevelIncreased  Level = "increased"
	LevelAggressive Level = "aggressive"
	LevelWarn       Level = "warn"
	LevelStop       Level = "stop"
)

// Tracker tracks per-session token usage against a budget and implements
// hooks.Hook so it can be attached to an agent's hook chain to run
// automatically around every model call.
type Tracker struct {
	mu            sync.Mutex
	maxTokens     int
	used          map[string]int
	baseThreshold int
}

// NewTracker creates a Tracker with the given token budget and base tool-result
// compression threshold (see internal/toolcompress.DefaultThresholdTokens for
// context on the latter). maxTokens <= 0 means "unlimited": Before never
// blocks, Ratio is always 0, and Level is always LevelNormal.
func NewTracker(maxTokens int, baseCompressionThreshold int) *Tracker {
	return &Tracker{
		maxTokens:     maxTokens,
		used:          make(map[string]int),
		baseThreshold: baseCompressionThreshold,
	}
}

// sessionKey resolves the bucket key for evt: the active session id from ctx,
// falling back to evt.Name (the model name) when no session is set so
// tracking still works in tests or other no-session contexts.
func sessionKey(ctx context.Context, evt *hooks.Event) string {
	if id := storage.SessionFromContext(ctx); id != "" {
		return id
	}
	return evt.Name
}

// Before implements hooks.Hook. It only acts on hooks.EventModelCallBefore,
// returning an error to abort the call once the resolved session has met or
// exceeded its token budget.
func (t *Tracker) Before(ctx context.Context, evt *hooks.Event) error {
	if evt.Type != hooks.EventModelCallBefore {
		return nil
	}
	if t.maxTokens <= 0 {
		return nil
	}
	sessionID := sessionKey(ctx, evt)
	t.mu.Lock()
	used := t.used[sessionID]
	t.mu.Unlock()
	if used >= t.maxTokens {
		return fmt.Errorf("token budget exceeded for session %q: used %d of %d tokens", sessionID, used, t.maxTokens)
	}
	return nil
}

// After implements hooks.Hook. It only acts on hooks.EventModelCallAfter,
// accumulating the response's token usage against the resolved session. It
// never blocks (always returns nil) since the call has already happened; a
// failed call (evt.Error != nil) or a non-*model.ChatResponse output
// contributes no usage.
func (t *Tracker) After(ctx context.Context, evt *hooks.Event) error {
	if evt.Type != hooks.EventModelCallAfter {
		return nil
	}
	if evt.Error != nil {
		return nil
	}
	resp, ok := evt.Output.(*model.ChatResponse)
	if !ok || resp == nil {
		return nil
	}
	sessionID := sessionKey(ctx, evt)
	t.mu.Lock()
	if t.used == nil {
		t.used = make(map[string]int)
	}
	t.used[sessionID] += resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	t.mu.Unlock()
	return nil
}

// Ratio returns the fraction of the token budget consumed by sessionID. It is
// always 0 when the tracker is unlimited (maxTokens <= 0), and is not clamped
// otherwise — callers may observe a value greater than 1.0 if usage overshot
// the budget between checks.
func (t *Tracker) Ratio(sessionID string) float64 {
	if t.maxTokens <= 0 {
		return 0
	}
	t.mu.Lock()
	used := t.used[sessionID]
	t.mu.Unlock()
	return float64(used) / float64(t.maxTokens)
}

// Level returns the current compression-ramp level for sessionID, derived
// from Ratio. An unlimited tracker (maxTokens <= 0) is always LevelNormal.
func (t *Tracker) Level(sessionID string) Level {
	if t.maxTokens <= 0 {
		return LevelNormal
	}
	ratio := t.Ratio(sessionID)
	switch {
	case ratio >= 1.0:
		return LevelStop
	case ratio >= 0.90:
		return LevelWarn
	case ratio >= 0.75:
		return LevelAggressive
	case ratio >= 0.50:
		return LevelIncreased
	default:
		return LevelNormal
	}
}

// CompressionThreshold returns the tool-result compression threshold (in
// tokens) that should be used for sessionID, scaled down from baseThreshold
// as Level increases: LevelNormal keeps baseThreshold as-is, LevelIncreased
// halves it, LevelAggressive quarters it, and LevelWarn/LevelStop use a small
// floor of max(baseThreshold/8, 50). The result is never <= 0: if the scaled
// value would otherwise be zero or negative (e.g. a tiny
// baseCompressionThreshold passed to NewTracker), it floors to 50.
func (t *Tracker) CompressionThreshold(sessionID string) int {
	var val int
	switch t.Level(sessionID) {
	case LevelIncreased:
		val = t.baseThreshold / 2
	case LevelAggressive:
		val = t.baseThreshold / 4
	case LevelWarn, LevelStop:
		val = t.baseThreshold / 8
		if val < 50 {
			val = 50
		}
	default:
		val = t.baseThreshold
	}
	if val <= 0 {
		val = 50
	}
	return val
}

// Used returns the total tokens accounted so far for sessionID.
func (t *Tracker) Used(sessionID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.used[sessionID]
}

// Remaining returns the tokens left in sessionID's budget, floored at 0. It
// returns -1 as a sentinel meaning "unlimited" when the tracker has no
// budget configured (maxTokens <= 0).
func (t *Tracker) Remaining(sessionID string) int {
	if t.maxTokens <= 0 {
		return -1
	}
	remaining := t.maxTokens - t.Used(sessionID)
	if remaining < 0 {
		remaining = 0
	}
	return remaining
}

// StatusLine renders a human-readable one-liner suitable for a TUI status
// bar, e.g. "budget: 12345 / 500000 tokens (2%) [normal]", or
// "budget: unlimited (12345 tokens used)" when the tracker has no budget
// configured.
func (t *Tracker) StatusLine(sessionID string) string {
	used := t.Used(sessionID)
	if t.maxTokens <= 0 {
		return fmt.Sprintf("budget: unlimited (%d tokens used)", used)
	}
	pct := int(t.Ratio(sessionID) * 100)
	return fmt.Sprintf("budget: %d / %d tokens (%d%%) [%s]", used, t.maxTokens, pct, t.Level(sessionID))
}
