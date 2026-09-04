package eval

import (
	"reflect"
	"testing"
)

func TestPPDExperiment(t *testing.T) {
	results := RunPPDExperiment()
	if want := 4 * 4 * 3; len(results) != want {
		t.Fatalf("RunPPDExperiment returned %d results, want %d", len(results), want)
	}
	if !reflect.DeepEqual(results, RunPPDExperiment()) {
		t.Fatal("RunPPDExperiment is not deterministic")
	}

	byArmCohort := make(map[PPDExperimentArm]map[string][]PPDExperimentResult)
	for _, result := range results {
		if result.Tokens <= 0 || result.ModelCalls <= 0 || result.ToolCalls <= 0 || result.WallTime <= 0 || !result.VerifiedSuccessful {
			t.Errorf("invalid result: %+v", result)
		}
		if byArmCohort[result.Arm] == nil {
			byArmCohort[result.Arm] = make(map[string][]PPDExperimentResult)
		}
		byArmCohort[result.Arm][result.Cohort] = append(byArmCohort[result.Arm][result.Cohort], result)
	}

	for _, arm := range []PPDExperimentArm{PPDArmA, PPDArmB, PPDArmC, PPDArmD} {
		for _, cohort := range []string{"simple", "medium", "cross_package", "forced_resume"} {
			if got := len(byArmCohort[arm][cohort]); got != 3 {
				t.Errorf("%s/%s has %d repeats, want 3", arm, cohort, got)
			}
		}
	}

	paired := make([]PairedResult, 0, 6)
	for _, cohort := range []string{"cross_package", "forced_resume"} {
		for repeat := 0; repeat < 3; repeat++ {
			baseline := byArmCohort[PPDArmA][cohort][repeat]
			candidate := byArmCohort[PPDArmD][cohort][repeat]
			paired = append(paired, PairedResult{
				Cohort:              cohort,
				BaselineSuccessful:  baseline.VerifiedSuccessful,
				CandidateSuccessful: candidate.VerifiedSuccessful,
				BaselineTokens:      float64(baseline.Tokens),
				CandidateTokens:     float64(candidate.Tokens),
				BaselineCalls:       float64(baseline.ModelCalls),
				CandidateCalls:      float64(candidate.ModelCalls),
			})
		}
	}
	accepted, err := registeredHypotheses(t).Decision.Accepts(paired)
	if err != nil || !accepted {
		t.Fatalf("ARM-D ablation accepted = %t, err = %v; want true, nil", accepted, err)
	}
}
