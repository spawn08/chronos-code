package eval

import (
	"os"
	"testing"
)

func registeredHypotheses(t *testing.T) HypothesisRegistry {
	t.Helper()
	data, err := os.ReadFile("../../benchmark/ppd/hypotheses.yaml")
	if err != nil {
		t.Fatalf("read hypothesis registry: %v", err)
	}
	registry, err := LoadHypothesisRegistry(data)
	if err != nil {
		t.Fatalf("load hypothesis registry: %v", err)
	}
	return registry
}

func TestHypothesisRegistryValidatesRegisteredArmsAndHypotheses(t *testing.T) {
	registry := registeredHypotheses(t)
	if err := registry.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestHypothesisRegistryRejectsUndefinedReferences(t *testing.T) {
	registry := registeredHypotheses(t)
	registry.Hypotheses[0].Metric = "unregistered_metric"
	if err := registry.Validate(); err == nil {
		t.Fatal("Validate accepted an undefined metric")
	}

	registry = registeredHypotheses(t)
	registry.Hypotheses[0].Controls = []string{"unregistered_control"}
	if err := registry.Validate(); err == nil {
		t.Fatal("Validate accepted an undefined control")
	}

	registry = registeredHypotheses(t)
	registry.Decision.MinTokenReductionPercent = 0
	if err := registry.Validate(); err == nil {
		t.Fatal("Validate accepted an undefined decision threshold")
	}
}

func TestHypothesisRunPinsRegistryContent(t *testing.T) {
	registry := registeredHypotheses(t)
	run, err := NewExperimentRun(registry, "H-001", "ARM-A")
	if err != nil {
		t.Fatalf("NewExperimentRun: %v", err)
	}
	registry.Hypotheses[0].Statement = "altered after the run started"
	if err := run.Validate(registry); err == nil {
		t.Fatal("run accepted altered registry content")
	}
}

func TestHypothesisDecisionRule(t *testing.T) {
	rule := registeredHypotheses(t).Decision
	accepted, err := rule.Accepts([]PairedResult{
		{Cohort: "cross_package", BaselineSuccessful: true, CandidateSuccessful: true, BaselineTokens: 100, CandidateTokens: 80, BaselineCalls: 10, CandidateCalls: 8},
		{Cohort: "forced_resume", BaselineSuccessful: true, CandidateSuccessful: true, BaselineTokens: 100, CandidateTokens: 80, BaselineCalls: 10, CandidateCalls: 8},
	})
	if err != nil || !accepted {
		t.Fatalf("Accepts = %t, %v; want true, nil", accepted, err)
	}

	accepted, err = rule.Accepts([]PairedResult{
		{Cohort: "cross_package", BaselineSuccessful: true, CandidateSuccessful: true, BaselineTokens: 100, CandidateTokens: 90, BaselineCalls: 10, CandidateCalls: 8},
		{Cohort: "forced_resume", BaselineSuccessful: true, CandidateSuccessful: true, BaselineTokens: 100, CandidateTokens: 80, BaselineCalls: 10, CandidateCalls: 8},
	})
	if err != nil || accepted {
		t.Fatalf("Accepts = %t, %v; want false, nil", accepted, err)
	}
}
