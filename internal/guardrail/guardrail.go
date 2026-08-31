// Package guardrail parses chronos-code's YAML guardrail configuration and
// converts it into chronos's own guardrails.Rule values (package
// github.com/spawn08/chronos/engine/guardrails). It is deliberately named
// "guardrail" (singular) so callers can import both packages side by side:
//
//	guardrails "github.com/spawn08/chronos/engine/guardrails"
//	"github.com/spawn08/chronos-code/internal/guardrail"
package guardrail

import (
	"context"
	"fmt"
	"regexp"

	guardrails "github.com/spawn08/chronos/engine/guardrails"
	"gopkg.in/yaml.v3"
)

const (
	defaultMaxTokensPerSession = 500000
	defaultWarnAtPercent       = 80
)

// RuleSpec describes a single guardrail rule as declared in YAML config.
type RuleSpec struct {
	Type      string   `yaml:"type"` // "max_length" | "blocklist" | "secret_detection"
	MaxChars  int      `yaml:"max_chars,omitempty"`
	Blocklist []string `yaml:"blocklist,omitempty"`
	Patterns  []string `yaml:"patterns,omitempty"` // only used when Type == "secret_detection"
}

// CostConfig describes session token budget limits.
type CostConfig struct {
	MaxTokensPerSession int `yaml:"max_tokens_per_session"`
	WarnAtPercent       int `yaml:"warn_at_percent"`
}

// Config is the top-level YAML guardrail configuration.
type Config struct {
	Name        string     `yaml:"name"`
	Description string     `yaml:"description"`
	Input       []RuleSpec `yaml:"input"`
	Output      []RuleSpec `yaml:"output"`
	Cost        CostConfig `yaml:"cost"`
}

// ParseConfig parses raw YAML bytes into a Config.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("guardrail: parsing config: %w", err)
	}
	return &cfg, nil
}

// TokenBudget returns the configured session token budget, falling back to
// sane defaults when the corresponding config field is zero.
func (c *Config) TokenBudget() (maxTokens, warnPercent int) {
	maxTokens = c.Cost.MaxTokensPerSession
	if maxTokens == 0 {
		maxTokens = defaultMaxTokensPerSession
	}
	warnPercent = c.Cost.WarnAtPercent
	if warnPercent == 0 {
		warnPercent = defaultWarnAtPercent
	}
	return maxTokens, warnPercent
}

// SecretGuardrail rejects content matching any of a set of regex patterns
// commonly associated with leaked secrets (API keys, tokens, private key
// headers, etc.).
type SecretGuardrail struct {
	patterns []*regexp.Regexp
	names    []string
}

// NewSecretGuardrail compiles the given regex patterns into a SecretGuardrail.
// If a pattern fails to compile, an error naming the offending pattern string
// and the underlying cause is returned.
func NewSecretGuardrail(patterns []string) (*SecretGuardrail, error) {
	g := &SecretGuardrail{
		patterns: make([]*regexp.Regexp, 0, len(patterns)),
		names:    make([]string, 0, len(patterns)),
	}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("guardrail: invalid secret detection pattern %q: %w", p, err)
		}
		g.patterns = append(g.patterns, re)
		g.names = append(g.names, p)
	}
	return g, nil
}

// Check implements guardrails.Guardrail. It fails on the first pattern that
// matches the content.
func (g *SecretGuardrail) Check(_ context.Context, content string) guardrails.Result {
	for i, re := range g.patterns {
		if re.MatchString(content) {
			return guardrails.Result{
				Passed: false,
				Reason: fmt.Sprintf("possible secret detected (pattern: %s)", g.names[i]),
			}
		}
	}
	return guardrails.Result{Passed: true}
}

// ruleFromSpec builds a guardrails.Guardrail from a single RuleSpec.
func ruleFromSpec(spec RuleSpec) (guardrails.Guardrail, error) {
	switch spec.Type {
	case "max_length":
		return &guardrails.MaxLengthGuardrail{MaxChars: spec.MaxChars}, nil
	case "blocklist":
		return &guardrails.BlocklistGuardrail{Blocklist: spec.Blocklist}, nil
	case "secret_detection":
		return NewSecretGuardrail(spec.Patterns)
	default:
		return nil, fmt.Errorf("guardrail: unknown rule type %q", spec.Type)
	}
}

// BuildRules converts a Config into a slice of guardrails.Rule values ready
// to be registered on a chronos guardrails.Engine.
//
// For each cfg.Input entry a guardrails.Rule is built with Position
// guardrails.Input, and for each cfg.Output entry with Position
// guardrails.Output. In addition, exactly one guardrails.Output rule named
// "secret-detection" is always appended, built from NewSecretGuardrail over
// the union of extraSecretPatterns and any spec.Patterns found among
// cfg.Output entries whose Type == "secret_detection".
//
// Edge case: if extraSecretPatterns is empty and no output spec supplies
// secret_detection patterns, the "secret-detection" rule is still added, but
// with zero compiled patterns — its Check will always pass since there is
// nothing to match against.
func BuildRules(cfg *Config, extraSecretPatterns []string) ([]guardrails.Rule, error) {
	rules := make([]guardrails.Rule, 0, len(cfg.Input)+len(cfg.Output)+1)

	for i, spec := range cfg.Input {
		gr, err := ruleFromSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("guardrail: building input rule %d (%s): %w", i, spec.Type, err)
		}
		rules = append(rules, guardrails.Rule{
			Name:      fmt.Sprintf("input-%s-%d", spec.Type, i),
			Position:  guardrails.Input,
			Guardrail: gr,
		})
	}

	mergedPatterns := make([]string, 0, len(extraSecretPatterns))
	mergedPatterns = append(mergedPatterns, extraSecretPatterns...)

	for i, spec := range cfg.Output {
		gr, err := ruleFromSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("guardrail: building output rule %d (%s): %w", i, spec.Type, err)
		}
		rules = append(rules, guardrails.Rule{
			Name:      fmt.Sprintf("output-%s-%d", spec.Type, i),
			Position:  guardrails.Output,
			Guardrail: gr,
		})
		if spec.Type == "secret_detection" {
			mergedPatterns = append(mergedPatterns, spec.Patterns...)
		}
	}

	secretGuardrail, err := NewSecretGuardrail(mergedPatterns)
	if err != nil {
		return nil, fmt.Errorf("guardrail: building secret-detection rule: %w", err)
	}
	rules = append(rules, guardrails.Rule{
		Name:      "secret-detection",
		Position:  guardrails.Output,
		Guardrail: secretGuardrail,
	})

	return rules, nil
}
