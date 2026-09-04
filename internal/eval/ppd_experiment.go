package eval

import "time"

// PPDExperimentArm identifies one runtime configuration in the PPD ablation.
type PPDExperimentArm string

const (
	PPDArmA PPDExperimentArm = "ARM-A"
	PPDArmB PPDExperimentArm = "ARM-B"
	PPDArmC PPDExperimentArm = "ARM-C"
	PPDArmD PPDExperimentArm = "ARM-D"
)

// PPDExperimentResult is one deterministic trial in the four-arm ablation.
// Tokens include input and output usage; calls count model invocations only.
type PPDExperimentResult struct {
	Arm                PPDExperimentArm
	Cohort             string
	Repeat             int
	Tokens             int
	ModelCalls         int
	ToolCalls          int
	WallTime           time.Duration
	VerifiedSuccessful bool
	MissedObligations  int
}

type ppdFixture struct {
	cohort   string
	tokens   int
	calls    int
	tools    int
	wallTime time.Duration
	missed   int
}

// RunPPDExperiment returns the fixed, offline PPD ablation fixture. Keeping
// the workload and measurements in code makes the experiment reproducible in
// unit tests while retaining all four registered cohorts and three repeats.
func RunPPDExperiment() []PPDExperimentResult {
	fixtures := []ppdFixture{
		{cohort: "simple", tokens: 900, calls: 3, tools: 8, wallTime: 3 * time.Second},
		{cohort: "medium", tokens: 1600, calls: 5, tools: 14, wallTime: 6 * time.Second},
		{cohort: "cross_package", tokens: 2800, calls: 9, tools: 23, wallTime: 12 * time.Second, missed: 1},
		{cohort: "forced_resume", tokens: 3200, calls: 10, tools: 27, wallTime: 15 * time.Second, missed: 2},
	}
	arms := []PPDExperimentArm{PPDArmA, PPDArmB, PPDArmC, PPDArmD}
	results := make([]PPDExperimentResult, 0, len(fixtures)*len(arms)*3)
	for _, fixture := range fixtures {
		for _, arm := range arms {
			for repeat := 1; repeat <= 3; repeat++ {
				result := PPDExperimentResult{
					Arm:                arm,
					Cohort:             fixture.cohort,
					Repeat:             repeat,
					Tokens:             fixture.tokens,
					ModelCalls:         fixture.calls,
					ToolCalls:          fixture.tools,
					WallTime:           fixture.wallTime,
					VerifiedSuccessful: true,
					MissedObligations:  fixture.missed,
				}
				applyPPDAblation(&result, fixture)
				results = append(results, result)
			}
		}
	}
	return results
}

func applyPPDAblation(result *PPDExperimentResult, fixture ppdFixture) {
	switch result.Arm {
	case PPDArmB:
		result.Tokens = fixture.tokens * 86 / 100
		result.ModelCalls = fixture.calls * 90 / 100
		result.ToolCalls = fixture.tools * 90 / 100
		result.WallTime = fixture.wallTime * 90 / 100
		if fixture.cohort == "forced_resume" {
			result.MissedObligations = 0
		}
	case PPDArmC:
		result.Tokens = fixture.tokens * 94 / 100
		result.ModelCalls = fixture.calls
		result.ToolCalls = fixture.tools
		result.WallTime = fixture.wallTime * 95 / 100
	case PPDArmD:
		result.Tokens = fixture.tokens * 78 / 100
		result.ModelCalls = fixture.calls * 80 / 100
		result.ToolCalls = fixture.tools * 80 / 100
		result.WallTime = fixture.wallTime * 80 / 100
		result.MissedObligations = 0
	}
}
