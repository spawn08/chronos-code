package guardrail

import (
	"context"
	"strings"
	"testing"

	guardrails "github.com/spawn08/chronos/engine/guardrails"
)

// defaultYAML is a verbatim copy of internal/defaults/guardrails/default.yaml.
const defaultYAML = `name: default
description: Standard guardrails for coding agents

input:
  - type: max_length
    max_chars: 100000

output:
  - type: max_length
    max_chars: 200000

cost:
  max_tokens_per_session: 500000
  warn_at_percent: 80
`

func TestParseConfig_Default(t *testing.T) {
	cfg, err := ParseConfig([]byte(defaultYAML))
	if err != nil {
		t.Fatalf("ParseConfig returned error: %v", err)
	}

	if cfg.Name != "default" {
		t.Errorf("Name = %q, want %q", cfg.Name, "default")
	}
	if len(cfg.Input) != 1 {
		t.Errorf("len(Input) = %d, want 1", len(cfg.Input))
	}
	if len(cfg.Output) != 1 {
		t.Errorf("len(Output) = %d, want 1", len(cfg.Output))
	}

	maxTokens, warnPercent := cfg.TokenBudget()
	if maxTokens != 500000 {
		t.Errorf("TokenBudget() maxTokens = %d, want 500000", maxTokens)
	}
	if warnPercent != 80 {
		t.Errorf("TokenBudget() warnPercent = %d, want 80", warnPercent)
	}
}

func TestTokenBudget_Defaults(t *testing.T) {
	cfg := &Config{} // zero-value Cost fields
	maxTokens, warnPercent := cfg.TokenBudget()
	if maxTokens != defaultMaxTokensPerSession {
		t.Errorf("maxTokens = %d, want default %d", maxTokens, defaultMaxTokensPerSession)
	}
	if warnPercent != defaultWarnAtPercent {
		t.Errorf("warnPercent = %d, want default %d", warnPercent, defaultWarnAtPercent)
	}
}

func TestTokenBudget_Overrides(t *testing.T) {
	cfg := &Config{Cost: CostConfig{MaxTokensPerSession: 1000, WarnAtPercent: 50}}
	maxTokens, warnPercent := cfg.TokenBudget()
	if maxTokens != 1000 {
		t.Errorf("maxTokens = %d, want 1000", maxTokens)
	}
	if warnPercent != 50 {
		t.Errorf("warnPercent = %d, want 50", warnPercent)
	}
}

func TestBuildRules_Count(t *testing.T) {
	cfg, err := ParseConfig([]byte(defaultYAML))
	if err != nil {
		t.Fatalf("ParseConfig returned error: %v", err)
	}

	rules, err := BuildRules(cfg, []string{"AKIA[0-9A-Z]{16}"})
	if err != nil {
		t.Fatalf("BuildRules returned error: %v", err)
	}

	wantCount := len(cfg.Input) + len(cfg.Output) + 1 // +1 for the always-added secret-detection rule
	if len(rules) != wantCount {
		t.Fatalf("len(rules) = %d, want %d", len(rules), wantCount)
	}

	var inputCount, outputCount int
	var sawSecretDetection bool
	for _, r := range rules {
		switch r.Position {
		case guardrails.Input:
			inputCount++
		case guardrails.Output:
			outputCount++
		default:
			t.Errorf("unexpected position %q for rule %q", r.Position, r.Name)
		}
		if r.Name == "secret-detection" {
			sawSecretDetection = true
			if r.Position != guardrails.Output {
				t.Errorf("secret-detection rule Position = %q, want %q", r.Position, guardrails.Output)
			}
		}
	}

	if inputCount != len(cfg.Input) {
		t.Errorf("inputCount = %d, want %d", inputCount, len(cfg.Input))
	}
	if outputCount != len(cfg.Output)+1 {
		t.Errorf("outputCount = %d, want %d", outputCount, len(cfg.Output)+1)
	}
	if !sawSecretDetection {
		t.Error("expected a rule named \"secret-detection\"")
	}
}

func TestBuildRules_NoPatterns(t *testing.T) {
	cfg := &Config{}
	rules, err := BuildRules(cfg, nil)
	if err != nil {
		t.Fatalf("BuildRules returned error: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("len(rules) = %d, want 1", len(rules))
	}
	// The lone secret-detection rule should always pass since it has no patterns.
	result := rules[0].Guardrail.Check(context.Background(), "anything at all, even AKIAABCDEFGHIJKLMNOP")
	if !result.Passed {
		t.Errorf("expected Check to pass with zero patterns, got Passed=false Reason=%q", result.Reason)
	}
}

func TestBuildRules_InvalidRegex(t *testing.T) {
	cfg := &Config{
		Output: []RuleSpec{
			{Type: "secret_detection", Patterns: []string{"("}}, // invalid regex
		},
	}
	_, err := BuildRules(cfg, nil)
	if err == nil {
		t.Fatal("expected error for invalid regex pattern, got nil")
	}
}

func TestBuildRules_UnknownType(t *testing.T) {
	cfg := &Config{
		Input: []RuleSpec{
			{Type: "not_a_real_type"},
		},
	}
	_, err := BuildRules(cfg, nil)
	if err == nil {
		t.Fatal("expected error for unknown rule type, got nil")
	}
}

func TestSecretGuardrail_Check(t *testing.T) {
	g, err := NewSecretGuardrail([]string{`AKIA[0-9A-Z]{16}`})
	if err != nil {
		t.Fatalf("NewSecretGuardrail returned error: %v", err)
	}

	dirty := "here is a key: AKIAABCDEFGHIJKLMNOP embedded in text"
	result := g.Check(context.Background(), dirty)
	if result.Passed {
		t.Error("expected Check to fail on content containing an AWS access key")
	}
	if !strings.Contains(result.Reason, "AKIA[0-9A-Z]{16}") {
		t.Errorf("Reason = %q, want it to mention the pattern", result.Reason)
	}

	clean := "nothing suspicious here"
	result = g.Check(context.Background(), clean)
	if !result.Passed {
		t.Errorf("expected Check to pass on clean content, got Reason=%q", result.Reason)
	}
}

func TestNewSecretGuardrail_InvalidPattern(t *testing.T) {
	_, err := NewSecretGuardrail([]string{"["})
	if err == nil {
		t.Fatal("expected error for invalid regex, got nil")
	}
}
