package learning

import (
	"testing"
	"time"
)

const sampleLearningYAML = `
distillation:
  model:
    provider: "anthropic"
    model: "claude-sonnet-4-6"
  prompt: |
    You analyze aggregated execution statistics.
`

func TestParse(t *testing.T) {
	cfg, err := Parse([]byte(sampleLearningYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Distillation.Model.Provider != "anthropic" {
		t.Errorf("Model.Provider = %q, want %q", cfg.Distillation.Model.Provider, "anthropic")
	}
	if cfg.Distillation.Model.Model != "claude-sonnet-4-6" {
		t.Errorf("Model.Model = %q, want %q", cfg.Distillation.Model.Model, "claude-sonnet-4-6")
	}
	if cfg.Distillation.Prompt == "" {
		t.Error("Prompt is empty, want the configured prompt text")
	}
}

func TestParse_Invalid(t *testing.T) {
	if _, err := Parse([]byte("not: [valid yaml")); err == nil {
		t.Error("Parse() with malformed YAML: want error, got nil")
	}
}

func TestConfig_ModelConfig(t *testing.T) {
	cfg, err := Parse([]byte(sampleLearningYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	mc := cfg.ModelConfig()
	if mc.Provider != "anthropic" || mc.Model != "claude-sonnet-4-6" {
		t.Errorf("ModelConfig() = %+v, want provider=anthropic model=claude-sonnet-4-6", mc)
	}
}

func TestReplayEvidenceRequiresVerifiedQualityAndPolicy(t *testing.T) {
	for _, tt := range []struct {
		name     string
		evidence ReplayEvidence
		want     bool
	}{
		{"accepted", ReplayEvidence{VerifiedOutcomes: 3, QualityPassed: true, PolicyPassed: true}, true},
		{"insufficient", ReplayEvidence{VerifiedOutcomes: 2, QualityPassed: true, PolicyPassed: true}, false},
		{"quality regression", ReplayEvidence{VerifiedOutcomes: 3, PolicyPassed: true}, false},
		{"policy regression", ReplayEvidence{VerifiedOutcomes: 3, QualityPassed: true}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.evidence.Acceptable(3); got != tt.want {
				t.Errorf("Acceptable(3) = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestStorePatternApprovalRetrievalAndRollback(t *testing.T) {
	store := NewStore(t.TempDir())
	candidate := PatternCandidate{RepoPath: "/repo", TriggerHash: triggerHash(NormalizeTrigger("fix bug")), SuccessCount: 3}
	replay := ReplayEvidence{VerifiedOutcomes: 3, QualityPassed: true, PolicyPassed: true}

	first, err := store.ApprovePattern(candidate, "rev-1", replay, 3)
	if err != nil {
		t.Fatalf("ApprovePattern() first error = %v", err)
	}
	if first.Version != 1 || !first.Current {
		t.Fatalf("first approval = %+v, want current version 1", first)
	}
	candidate.SolutionSummary = "second measured behavior"
	second, err := store.ApprovePattern(candidate, "rev-1", replay, 3)
	if err != nil {
		t.Fatalf("ApprovePattern() second error = %v", err)
	}
	if second.Version != 2 {
		t.Fatalf("second approval version = %d, want 2", second.Version)
	}

	current, err := store.CurrentPattern("/repo", "FIX bug!", "rev-1")
	if err != nil || current == nil || current.Version != 2 {
		t.Fatalf("CurrentPattern() = %+v, %v; want version 2", current, err)
	}
	stale, err := store.CurrentPattern("/repo", "fix bug", "rev-2")
	if err != nil || stale != nil {
		t.Fatalf("CurrentPattern() stale = %+v, %v; want nil", stale, err)
	}

	rolledBack, err := store.RollbackPattern("/repo", "fix bug")
	if err != nil {
		t.Fatalf("RollbackPattern() error = %v", err)
	}
	if rolledBack.Version != 1 || !rolledBack.Current {
		t.Fatalf("RollbackPattern() = %+v, want current version 1", rolledBack)
	}
	current, err = store.CurrentPattern("/repo", "fix bug", "rev-1")
	if err != nil || current == nil || current.Version != 1 {
		t.Fatalf("CurrentPattern() after rollback = %+v, %v; want version 1", current, err)
	}
}

func TestStoreRejectsInsufficientOrRegressivePatternReplay(t *testing.T) {
	store := NewStore(t.TempDir())
	candidate := PatternCandidate{RepoPath: "/repo", TriggerHash: triggerHash("fix bug"), SuccessCount: 3, LastUsedAt: time.Now()}
	for _, replay := range []ReplayEvidence{
		{VerifiedOutcomes: 2, QualityPassed: true, PolicyPassed: true},
		{VerifiedOutcomes: 3, QualityPassed: false, PolicyPassed: true},
		{VerifiedOutcomes: 3, QualityPassed: true, PolicyPassed: false},
	} {
		if _, err := store.ApprovePattern(candidate, "rev", replay, 3); err == nil {
			t.Errorf("ApprovePattern(%+v) error = nil, want replay gate rejection", replay)
		}
	}
}
