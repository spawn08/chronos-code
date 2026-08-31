package learning

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos/engine/model"
)

// Suggestion is a distilled candidate — either a new specialist agent
// definition or a standalone pattern note — awaiting human review (PRD
// P3-002/003). Nothing built from a Suggestion is ever wired into the
// active agent set until `chronos-code learn accept` runs.
type Suggestion struct {
	ID             string    `yaml:"id"`
	Kind           string    `yaml:"kind"` // "agent" or "pattern"
	AgentID        string    `yaml:"agent_id,omitempty"`
	Title          string    `yaml:"title"`
	Rationale      string    `yaml:"rationale"`
	Confidence     float64   `yaml:"confidence"`
	YAML           string    `yaml:"yaml"`
	SourceAgentID  string    `yaml:"source_agent_id"`
	SourceSessions []string  `yaml:"source_sessions"`
	CreatedAt      time.Time `yaml:"created_at"`
}

// modelResponse is the structured shape the distillation prompt (see
// learning.yaml's distillation.prompt) asks the model to reply with.
type modelResponse struct {
	Kind       string  `yaml:"kind"`
	AgentID    string  `yaml:"agent_id"`
	Title      string  `yaml:"title"`
	Rationale  string  `yaml:"rationale"`
	Confidence float64 `yaml:"confidence"`
	YAML       string  `yaml:"yaml"`
}

// Distiller turns an aggregated Report into a Suggestion using an LLM (PRD
// P3-002).
type Distiller struct {
	provider model.Provider
	modelID  string
	prompt   string
}

// NewDistiller builds a Distiller from provider (already constructed by the
// caller via sdk/agent.BuildProvider, matching how internal/router builds
// its T1 classifier) and cfg's distillation model/prompt.
func NewDistiller(provider model.Provider, cfg *Config) *Distiller {
	return &Distiller{
		provider: provider,
		modelID:  cfg.Distillation.Model.Model,
		prompt:   cfg.Distillation.Prompt,
	}
}

// Suggest sends report.Summary() (aggregated counts only, never raw trace
// payloads) to the distillation model and parses its YAML reply into a
// Suggestion. It returns (nil, nil) — not an error — when the model
// recommends no action ("kind: none"), so callers don't need to
// special-case that outcome.
func (d *Distiller) Suggest(ctx context.Context, report *Report) (*Suggestion, error) {
	resp, err := d.provider.Chat(ctx, &model.ChatRequest{
		Model:     d.modelID,
		MaxTokens: 2000,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: d.prompt},
			{Role: model.RoleUser, Content: report.Summary()},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("learning: distill: %w", err)
	}

	var mr modelResponse
	if err := yaml.Unmarshal([]byte(stripCodeFence(resp.Content)), &mr); err != nil {
		return nil, fmt.Errorf("learning: parse distillation response: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(mr.Kind), "none") || strings.TrimSpace(mr.YAML) == "" {
		return nil, nil
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}
	return &Suggestion{
		ID:             id,
		Kind:           mr.Kind,
		AgentID:        mr.AgentID,
		Title:          mr.Title,
		Rationale:      mr.Rationale,
		Confidence:     mr.Confidence,
		YAML:           mr.YAML,
		SourceAgentID:  report.AgentID,
		SourceSessions: report.Sessions,
		CreatedAt:      time.Now(),
	}, nil
}

// stripCodeFence removes a leading/trailing ``` or ```yaml fence, in case
// the model wraps its reply in one despite the prompt asking for plain YAML.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 1 {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func newID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("learning: generate id: %w", err)
	}
	return "learn_" + hex.EncodeToString(buf), nil
}
