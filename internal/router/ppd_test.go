package router

import (
	"errors"
	"testing"

	"github.com/spawn08/chronos-code/internal/defaults"
)

type fakePPDFallback struct {
	delegate bool
	err      error
	calls    int
}

func (f *fakePPDFallback) ShouldDelegate(PPDRequest) (bool, error) {
	f.calls++
	return f.delegate, f.err
}

func TestPPDPolicy_SimpleTasksBypass(t *testing.T) {
	policy := NewPPDPolicy(testPPDConfig(PPDModeEnabled), nil)
	for _, tc := range []struct {
		name    string
		request PPDRequest
	}{
		{"one-file edit", PPDRequest{FileCount: 1, Kind: TaskKindEdit}},
		{"explanation", PPDRequest{Kind: TaskKindExplain}},
		{"search", PPDRequest{Kind: TaskKind("search")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := policy.Decide(tc.request)
			if decision.Action != PPDActionBypass || decision.Reason != "simple_task" {
				t.Errorf("Decide(%+v) = (%q, %q), want (bypass, simple_task)", tc.request, decision.Action, decision.Reason)
			}
		})
	}
}

func TestPPDPolicy_ComplexTasksDelegate(t *testing.T) {
	policy := NewPPDPolicy(testPPDConfig(PPDModeEnabled), nil)
	cases := []struct {
		name    string
		request PPDRequest
		reason  string
	}{
		{"cross package", PPDRequest{PackageCount: 2}, "package_breadth"},
		{"multi stage", PPDRequest{EstimatedCalls: 5}, "multi_stage"},
		{"forced resume", PPDRequest{ResumeLikely: true}, "resume_likely"},
		{"explicit", PPDRequest{ExplicitPPD: true}, "explicit_ppd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := policy.Decide(tc.request)
			if decision.Action != PPDActionDelegate || decision.Reason != tc.reason {
				t.Fatalf("Decide(%+v) = (%q, %q), want (delegate, %q)", tc.request, decision.Action, decision.Reason, tc.reason)
			}
		})
	}
}

func TestPPDPolicy_ShadowAndObservability(t *testing.T) {
	policy := NewPPDPolicy(testPPDConfig(PPDModeShadow), nil)
	decision := policy.Decide(PPDRequest{PackageCount: 2, OracleCohort: "simple", OracleAction: PPDActionBypass})
	if decision.Action != PPDActionShadow || decision.Reason != "package_breadth" || !decision.FalsePositive {
		t.Fatalf("Decide() = %+v, want observable shadow false positive", decision)
	}
	if decision.ClassifierVersion != "v1" || decision.Features.PackageCount != 2 || decision.Thresholds.MinPackages != 2 || decision.Specialist != "ppd-planner" || decision.OracleCohort != "simple" {
		t.Errorf("Decision observability fields = %+v", decision)
	}
}

func TestPPDPolicy_PlannerLimitAndFallbackFailureBypass(t *testing.T) {
	config := testPPDConfig(PPDModeEnabled)
	policy := NewPPDPolicy(config, &fakePPDFallback{delegate: true})
	decision := policy.Decide(PPDRequest{ExplicitPPD: true, PlannerCalls: config.MaxPlannerCalls})
	if decision.Action != PPDActionBypass || decision.Reason != "planner_call_limit" {
		t.Fatalf("planner limit decision = %+v, want bypass", decision)
	}

	fallback := &fakePPDFallback{err: errors.New("unavailable")}
	policy = NewPPDPolicy(config, fallback)
	decision = policy.Decide(PPDRequest{EstimatedCalls: config.Thresholds.MinEstimatedCalls - 1})
	if decision.Action != PPDActionBypass || decision.Reason != "fallback_failed" || fallback.calls != 1 {
		t.Errorf("fallback failure decision = %+v, calls = %d; want bypass, one call", decision, fallback.calls)
	}
}

func TestPPDPolicy_BundledConfigDefaultsToShadow(t *testing.T) {
	data, err := defaults.ReadFile("routing.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	decision := NewPPDPolicy(cfg.PPD, nil).Decide(PPDRequest{PackageCount: cfg.PPD.Thresholds.MinPackages})
	if decision.Action != PPDActionShadow || decision.ClassifierVersion != cfg.PPD.Version {
		t.Errorf("bundled policy decision = %+v, want shadow with version %q", decision, cfg.PPD.Version)
	}
}

func testPPDConfig(mode PPDMode) PPDConfig {
	return PPDConfig{
		Version: "v1", Mode: mode, Specialist: "ppd-planner", MaxPlannerCalls: 1,
		Thresholds: PPDThresholds{MinFiles: 3, MinPackages: 2, MinEstimatedCalls: 5},
	}
}
