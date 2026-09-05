// Package budget implements PRD P2-009: real-time per-session token/cost
// budget tracking with a progressive compression ramp. Tracker is a
// chronos hooks.Hook: wiring it into an agent's hook chain is the only
// integration point required — it observes every model call to accumulate
// usage (After) and blocks further calls once a session's budget is
// exhausted (Before), with no other package needing to know it exists.
package budget

import (
	"context"
	"errors"
	"fmt"
	"math"
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

// Microdollars is one millionth of a US dollar. Using integer units keeps
// model pricing deterministic and avoids floating-point rounding.
type Microdollars int64

// ModelPrice is a model's input and output price per token.
type ModelPrice struct {
	InputMicrodollarsPerToken  Microdollars
	OutputMicrodollarsPerToken Microdollars
}

// SessionCost is an atomic snapshot of one session's reconciled usage and
// outstanding pre-call reservations.
type SessionCost struct {
	InputTokens          int64
	OutputTokens         int64
	CacheReadTokens      int64
	CacheCreationTokens  int64
	SpentMicrodollars    Microdollars
	ReservedMicrodollars Microdollars
}

// ReservationID identifies an outstanding pre-call cost reservation.
type ReservationID uint64

var (
	// ErrUnknownModel indicates that deterministic pricing is unavailable.
	ErrUnknownModel = errors.New("unknown model price")
	// ErrUSDBudgetExceeded indicates that a reservation would exceed the cap.
	ErrUSDBudgetExceeded = errors.New("USD budget exceeded")
	// ErrUnknownReservation indicates that a reservation was already reconciled
	// or was not created by this tracker.
	ErrUnknownReservation = errors.New("unknown cost reservation")
)

var bundledModelPrices = map[string]ModelPrice{
	"claude-haiku-4-5":  {InputMicrodollarsPerToken: 1, OutputMicrodollarsPerToken: 5},
	"claude-sonnet-4-6": {InputMicrodollarsPerToken: 3, OutputMicrodollarsPerToken: 15},
	"claude-opus-4-8":   {InputMicrodollarsPerToken: 5, OutputMicrodollarsPerToken: 25},
}

// PriceForModel returns deterministic pricing for a bundled routing model.
func PriceForModel(modelID string) (ModelPrice, error) {
	price, ok := bundledModelPrices[modelID]
	if !ok {
		return ModelPrice{}, fmt.Errorf("%w: %q", ErrUnknownModel, modelID)
	}
	return price, nil
}

type reservation struct {
	sessionID string
	price     ModelPrice
	cost      Microdollars
}

// Tracker tracks per-session token usage against a budget and implements
// hooks.Hook so it can be attached to an agent's hook chain to run
// automatically around every model call.
type Tracker struct {
	mu            sync.Mutex
	maxTokens     int
	used          map[string]int
	baseThreshold int
	usdCap        Microdollars
	costs         map[string]SessionCost
	reservations  map[ReservationID]reservation
	nextReserveID ReservationID
}

// NewTracker creates a Tracker with the given token budget and base tool-result
// compression threshold (see internal/toolcompress.DefaultThresholdTokens for
// context on the latter). maxTokens <= 0 means "unlimited": Before never
// blocks, Ratio is always 0, and Level is always LevelNormal.
func NewTracker(maxTokens int, baseCompressionThreshold int) *Tracker {
	return NewTrackerWithUSDCap(maxTokens, baseCompressionThreshold, 0)
}

// NewTrackerWithUSDCap creates a Tracker with token and USD budgets. A usdCap
// <= 0 means unlimited USD spend; known-model pricing and accounting remain
// active so callers can still report session cost.
func NewTrackerWithUSDCap(maxTokens int, baseCompressionThreshold int, usdCap Microdollars) *Tracker {
	return &Tracker{
		maxTokens:     maxTokens,
		used:          make(map[string]int),
		baseThreshold: baseCompressionThreshold,
		usdCap:        usdCap,
		costs:         make(map[string]SessionCost),
		reservations:  make(map[ReservationID]reservation),
	}
}

// Reserve conservatively reserves the cost of estimated input and maximum
// output tokens before a model call. The cap check and reservation are atomic.
func (t *Tracker) Reserve(sessionID, modelID string, estimatedInputTokens, maxOutputTokens int) (ReservationID, error) {
	price, err := PriceForModel(modelID)
	if err != nil {
		return 0, err
	}
	cost, err := price.cost(estimatedInputTokens, maxOutputTokens)
	if err != nil {
		return 0, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	current := t.costs[sessionID]
	if t.usdCap > 0 && exceedsCap(current.SpentMicrodollars, current.ReservedMicrodollars, cost, t.usdCap) {
		return 0, fmt.Errorf("%w for session %q: spent %d, reserved %d, requested %d, cap %d microdollars",
			ErrUSDBudgetExceeded, sessionID, current.SpentMicrodollars, current.ReservedMicrodollars, cost, t.usdCap)
	}
	t.nextReserveID++
	id := t.nextReserveID
	current.ReservedMicrodollars += cost
	t.costs[sessionID] = current
	t.reservations[id] = reservation{sessionID: sessionID, price: price, cost: cost}
	return id, nil
}

// Reconcile replaces an outstanding reservation with the call's actual input
// and output usage. Passing zero usage releases a reservation for a failed
// call. Actual spend is recorded even when it exceeds the original estimate.
func (t *Tracker) Reconcile(id ReservationID, inputTokens, outputTokens int) error {
	return t.ReconcileUsage(id, model.Usage{PromptTokens: inputTokens, CompletionTokens: outputTokens})
}

// ReconcileUsage replaces an outstanding reservation with the call's actual
// usage, applying cache-read (10%) and cache-write (125%) input pricing.
func (t *Tracker) ReconcileUsage(id ReservationID, usage model.Usage) error {
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.CacheReadTokens < 0 || usage.CacheCreationTokens < 0 {
		return fmt.Errorf("token counts must be non-negative: %+v", usage)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	reserved, ok := t.reservations[id]
	if !ok {
		return fmt.Errorf("%w: %d", ErrUnknownReservation, id)
	}
	actual, err := reserved.price.costWithCache(usage)
	if err != nil {
		return err
	}
	current := t.costs[reserved.sessionID]
	current.ReservedMicrodollars -= reserved.cost
	current.SpentMicrodollars += actual
	current.InputTokens += int64(usage.UncachedPromptTokens())
	current.OutputTokens += int64(usage.CompletionTokens)
	current.CacheReadTokens += int64(usage.CacheReadTokens)
	current.CacheCreationTokens += int64(usage.CacheCreationTokens)
	t.costs[reserved.sessionID] = current
	delete(t.reservations, id)
	return nil
}

// Cost returns an atomic cost and usage snapshot for sessionID.
func (t *Tracker) Cost(sessionID string) SessionCost {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.costs[sessionID]
}

// HasUSDCap reports whether deterministic pricing is required to enforce a
// configured monetary limit.
func (t *Tracker) HasUSDCap() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.usdCap > 0
}

func (p ModelPrice) cost(inputTokens, outputTokens int) (Microdollars, error) {
	if inputTokens < 0 || outputTokens < 0 {
		return 0, fmt.Errorf("token counts must be non-negative: input %d, output %d", inputTokens, outputTokens)
	}
	input := int64(inputTokens)
	output := int64(outputTokens)
	if input > math.MaxInt64/int64(p.InputMicrodollarsPerToken) || output > math.MaxInt64/int64(p.OutputMicrodollarsPerToken) {
		return 0, errors.New("model cost overflows microdollars")
	}
	inputCost := input * int64(p.InputMicrodollarsPerToken)
	outputCost := output * int64(p.OutputMicrodollarsPerToken)
	if inputCost > math.MaxInt64-outputCost {
		return 0, errors.New("model cost overflows microdollars")
	}
	return Microdollars(inputCost + outputCost), nil
}

func (p ModelPrice) costWithCache(usage model.Usage) (Microdollars, error) {
	uncached := usage.UncachedPromptTokens()
	base, err := p.cost(uncached, usage.CompletionTokens)
	if err != nil {
		return 0, err
	}
	write := int64(usage.CacheCreationTokens)
	read := int64(usage.CacheReadTokens)
	inRate := int64(p.InputMicrodollarsPerToken)
	if write > 0 && inRate > math.MaxInt64/write/5 {
		return 0, errors.New("model cost overflows microdollars")
	}
	writeCost := write * inRate * 5 / 4
	readCost := int64(0)
	if read > 0 {
		readCost = read * inRate / 10
	}
	if writeCost > math.MaxInt64-int64(base) || readCost > math.MaxInt64-int64(base)-writeCost {
		return 0, errors.New("model cost overflows microdollars")
	}
	return base + Microdollars(writeCost+readCost), nil
}

func exceedsCap(spent, reserved, requested, cap Microdollars) bool {
	return spent > cap-reserved || requested > cap-spent-reserved
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
	t.used[sessionID] += billedSessionTokens(resp.Usage)
	t.mu.Unlock()
	return nil
}

// billedSessionTokens is incremental spend for the operational governor:
// uncached prompt tokens, cache writes, and completion. Cache hits occupy
// the model window and are billed at a discount (see ReconcileUsage), but
// summing PromptWindowTokens() across rounds recounts the same cached prefix
// on every tool-loop call and exhausts max_tokens_per_session far below
// actual unique usage.
func billedSessionTokens(u model.Usage) int {
	return u.UncachedPromptTokens() + u.CacheCreationTokens + u.CompletionTokens
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

// ResetSession clears sessionID's accounted token usage, letting it keep
// making model calls under the same budget cap. It does not touch
// sessionID's dollar-cost accounting (Cost/SessionCost) — that reflects
// money actually spent and stays accurate regardless of this operational
// governor being reset. Intended for a caller that has just compacted the
// session's conversation history to recover from a budget cap (rather than
// discarding the session outright) and wants the cap check in Before to
// stop blocking further calls on it.
func (t *Tracker) ResetSession(sessionID string) {
	t.mu.Lock()
	delete(t.used, sessionID)
	t.mu.Unlock()
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
