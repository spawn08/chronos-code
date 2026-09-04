package router

import "fmt"

// PPDMode controls whether qualifying requests are recorded only or sent to
// the PPD specialist.
type PPDMode string

const (
	PPDModeDisabled PPDMode = "disabled"
	PPDModeShadow   PPDMode = "shadow"
	PPDModeEnabled  PPDMode = "enabled"
)

// PPDAction is the observable result of applying the complexity policy.
type PPDAction string

const (
	PPDActionBypass   PPDAction = "bypass"
	PPDActionShadow   PPDAction = "shadow"
	PPDActionDelegate PPDAction = "delegate"
)

// PPDThresholds holds the versioned limits used to qualify a request.
type PPDThresholds struct {
	MinFiles          int `yaml:"min_files"`
	MinPackages       int `yaml:"min_packages"`
	MinEstimatedCalls int `yaml:"min_estimated_calls"`
}

// PPDConfig is the routing.yaml policy section. Enabled delegates qualifying
// work to the specialist. Shadow preserves a decision without invoking it.
type PPDConfig struct {
	Version         string        `yaml:"version"`
	Mode            PPDMode       `yaml:"mode"`
	Specialist      string        `yaml:"specialist"`
	MaxPlannerCalls int           `yaml:"max_planner_calls"`
	Thresholds      PPDThresholds `yaml:"thresholds"`
}

// PPDRequest contains metadata collected before the specialist is considered.
// OracleCohort and OracleAction are evaluation labels, not policy inputs.
type PPDRequest struct {
	ExplicitPPD    bool
	FileCount      int
	PackageCount   int
	Kind           TaskKind
	HighRisk       bool
	EstimatedCalls int
	ResumeLikely   bool
	PlannerCalls   int
	OracleCohort   string
	OracleAction   PPDAction
}

// PPDFeatures records the complete deterministic feature vector used for a
// decision so false positives can be analyzed by oracle cohort.
type PPDFeatures struct {
	ExplicitPPD    bool
	FileCount      int
	PackageCount   int
	Kind           TaskKind
	HighRisk       bool
	EstimatedCalls int
	ResumeLikely   bool
}

// PPDDecision is retained with routing telemetry before a PPD specialist can
// be called. A false positive is a policy delegate/shadow against a bypass
// oracle label.
type PPDDecision struct {
	Action            PPDAction
	ClassifierVersion string
	Features          PPDFeatures
	Thresholds        PPDThresholds
	Reason            string
	Specialist        string
	OracleCohort      string
	FalsePositive     bool
}

// PPDFallback can resolve an otherwise ambiguous boundary request. It is
// optional; failures deliberately preserve the normal coder path.
type PPDFallback interface {
	ShouldDelegate(PPDRequest) (bool, error)
}

// PPDPolicy applies the configured complexity policy without performing any
// model calls itself.
type PPDPolicy struct {
	config   PPDConfig
	fallback PPDFallback
}

func NewPPDPolicy(config PPDConfig, fallback PPDFallback) *PPDPolicy {
	return &PPDPolicy{config: config, fallback: fallback}
}

// Decide selects bypass, shadow, or delegate. A fallback is considered only
// at the estimated-call boundary, keeping ordinary simple requests at T0.
func (p *PPDPolicy) Decide(request PPDRequest) PPDDecision {
	decision := PPDDecision{
		Action:            PPDActionBypass,
		ClassifierVersion: p.config.Version,
		Features: PPDFeatures{
			ExplicitPPD: request.ExplicitPPD, FileCount: request.FileCount,
			PackageCount: request.PackageCount, Kind: request.Kind, HighRisk: request.HighRisk,
			EstimatedCalls: request.EstimatedCalls, ResumeLikely: request.ResumeLikely,
		},
		Thresholds:   p.config.Thresholds,
		Specialist:   p.config.Specialist,
		OracleCohort: request.OracleCohort,
	}

	if p.config.Mode == PPDModeDisabled {
		decision.Reason = "policy_disabled"
		return p.withOracle(decision, request.OracleAction)
	}
	if p.config.MaxPlannerCalls > 0 && request.PlannerCalls >= p.config.MaxPlannerCalls {
		decision.Reason = "planner_call_limit"
		return p.withOracle(decision, request.OracleAction)
	}

	qualifies, reason := p.qualifies(request)
	if !qualifies && p.fallback != nil && p.config.Thresholds.MinEstimatedCalls > 0 && request.EstimatedCalls == p.config.Thresholds.MinEstimatedCalls-1 {
		delegate, err := p.fallback.ShouldDelegate(request)
		if err != nil {
			decision.Reason = "fallback_failed"
			return p.withOracle(decision, request.OracleAction)
		}
		if delegate {
			qualifies, reason = true, "fallback_delegate"
		}
	}
	if !qualifies {
		decision.Reason = "simple_task"
		return p.withOracle(decision, request.OracleAction)
	}

	decision.Reason = reason
	switch p.config.Mode {
	case PPDModeShadow:
		decision.Action = PPDActionShadow
	case PPDModeEnabled:
		decision.Action = PPDActionDelegate
	default:
		decision.Reason = fmt.Sprintf("unsupported_mode:%s", p.config.Mode)
	}
	return p.withOracle(decision, request.OracleAction)
}

func (p *PPDPolicy) qualifies(request PPDRequest) (bool, string) {
	if request.ExplicitPPD {
		return true, "explicit_ppd"
	}
	if request.ResumeLikely {
		return true, "resume_likely"
	}
	if request.HighRisk {
		return true, "high_risk"
	}
	if p.config.Thresholds.MinPackages > 0 && request.PackageCount >= p.config.Thresholds.MinPackages {
		return true, "package_breadth"
	}
	if p.config.Thresholds.MinFiles > 0 && request.FileCount >= p.config.Thresholds.MinFiles {
		return true, "file_breadth"
	}
	if p.config.Thresholds.MinEstimatedCalls > 0 && request.EstimatedCalls >= p.config.Thresholds.MinEstimatedCalls {
		return true, "multi_stage"
	}
	return false, ""
}

func (p *PPDPolicy) withOracle(decision PPDDecision, oracle PPDAction) PPDDecision {
	decision.FalsePositive = (decision.Action == PPDActionShadow || decision.Action == PPDActionDelegate) && oracle == PPDActionBypass
	return decision
}
