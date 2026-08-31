package router

import (
	"context"
	"fmt"
	"strings"

	"github.com/spawn08/chronos/engine/model"
)

// Classifier is implemented by T1 fallback classifiers (T1Classifier below).
// It exists so Router.ClassifyWithFallback doesn't need to know how the
// fallback is actually computed, just that it returns an intent name.
type Classifier interface {
	Classify(ctx context.Context, message string) (intent string, err error)
}

// T1Classifier asks a cheap (T1) model to classify a message that the
// deterministic T0 patterns didn't match, per PRD P2-006 / routing.yaml's
// classification_prompt. It implements Classifier.
type T1Classifier struct {
	provider  model.Provider
	modelID   string
	maxTokens int
	prompt    string
}

// NewT1Classifier builds a T1Classifier from cfg.Router's model and
// classification_prompt. provider is built by the caller (chronos-code's
// orchestrator already knows how to turn a provider name into a
// model.Provider via sdk/agent.BuildProvider) — this package stays decoupled
// from that construction logic. Returns nil if cfg has no classification
// prompt configured, so callers can skip SetT1 without a nil check of their
// own.
func NewT1Classifier(provider model.Provider, cfg *Config) *T1Classifier {
	if provider == nil || strings.TrimSpace(cfg.Router.ClassificationPrompt) == "" {
		return nil
	}
	maxTokens := cfg.Router.BudgetTokens
	if maxTokens <= 0 {
		maxTokens = 20
	}
	return &T1Classifier{
		provider:  provider,
		modelID:   cfg.Router.Model.Model,
		maxTokens: maxTokens,
		prompt:    cfg.Router.ClassificationPrompt,
	}
}

// Classify sends message to the T1 model with cfg.Router.ClassificationPrompt
// as the system prompt and returns its answer, lowercased and trimmed. The
// result is otherwise unvalidated — callers must check it against their own
// intent set (Router.ClassifyWithFallback checks it against intentAgents).
func (c *T1Classifier) Classify(ctx context.Context, message string) (string, error) {
	resp, err := c.provider.Chat(ctx, &model.ChatRequest{
		Model:     c.modelID,
		MaxTokens: c.maxTokens,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: c.prompt},
			{Role: model.RoleUser, Content: message},
		},
	})
	if err != nil {
		return "", fmt.Errorf("router: t1 classify: %w", err)
	}
	return strings.ToLower(strings.TrimSpace(resp.Content)), nil
}

// SetT1 attaches a T1 fallback classifier, used by ClassifyWithFallback when
// no T0 pattern matches. Passing nil disables the fallback.
func (r *Router) SetT1(c Classifier) {
	r.t1 = c
}

// ClassifyWithFallback first tries deterministic T0 matching (Classify). If
// nothing matches and a T1 fallback classifier is attached (SetT1), it asks
// the cheap model to classify message and maps its answer back to an agent
// via the same intent_routing table used for T0. Any T1 failure (no
// classifier configured, provider error, or an intent the model invented
// that isn't in intent_routing) is treated the same as "still unmatched" —
// callers never need to special-case whether T1 is configured.
func (r *Router) ClassifyWithFallback(ctx context.Context, message string) (intent, agentID string, matched bool) {
	if intent, agentID, matched = r.Classify(message); matched {
		return intent, agentID, true
	}
	if r.t1 == nil {
		return intent, agentID, false
	}
	t1Intent, err := r.t1.Classify(ctx, message)
	if err != nil {
		return intent, agentID, false
	}
	if agentID, ok := r.intentAgents[t1Intent]; ok {
		return t1Intent, agentID, true
	}
	return intent, agentID, false
}
