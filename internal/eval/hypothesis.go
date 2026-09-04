package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// HypothesisRegistryVersion identifies the supported hypothesis registry schema.
const HypothesisRegistryVersion = "v1"

// HypothesisRegistry is the pre-registered experiment contract for BL-018.
type HypothesisRegistry struct {
	Version    string       `yaml:"version" json:"version"`
	Decision   DecisionRule `yaml:"decision_rule" json:"decision_rule"`
	Metrics    []Metric     `yaml:"metrics" json:"metrics"`
	Cohorts    []Cohort     `yaml:"cohorts" json:"cohorts"`
	Controls   []string     `yaml:"controls" json:"controls"`
	Repeats    int          `yaml:"repeats" json:"repeats"`
	Arms       []Arm        `yaml:"arms" json:"arms"`
	Hypotheses []Hypothesis `yaml:"hypotheses" json:"hypotheses"`
}

type DecisionRule struct {
	PrimaryMetric            string   `yaml:"primary_metric" json:"primary_metric"`
	CoPrimaryMetric          string   `yaml:"co_primary_metric" json:"co_primary_metric"`
	SuccessMetric            string   `yaml:"success_metric" json:"success_metric"`
	RequiredCohorts          []string `yaml:"required_cohorts" json:"required_cohorts"`
	MinTokenReductionPercent float64  `yaml:"min_token_reduction_percent" json:"min_token_reduction_percent"`
	MinCallReductionPercent  float64  `yaml:"min_call_reduction_percent" json:"min_call_reduction_percent"`
	MaxSuccessRegressionPP   float64  `yaml:"max_success_regression_pp" json:"max_success_regression_pp"`
}

type Metric struct {
	ID string `yaml:"id" json:"id"`
}

type Cohort struct {
	ID string `yaml:"id" json:"id"`
}

type Arm struct {
	ID          string `yaml:"id" json:"id"`
	Description string `yaml:"description" json:"description"`
}

type Hypothesis struct {
	ID        string   `yaml:"id" json:"id"`
	Statement string   `yaml:"statement" json:"statement"`
	Metric    string   `yaml:"metric" json:"metric"`
	Cohorts   []string `yaml:"cohorts" json:"cohorts"`
	Controls  []string `yaml:"controls" json:"controls"`
	Ablation  string   `yaml:"ablation" json:"ablation"`
	Guardrail string   `yaml:"guardrail" json:"guardrail"`
}

// LoadHypothesisRegistry decodes a versioned registry and rejects unknown schema fields.
func LoadHypothesisRegistry(data []byte) (HypothesisRegistry, error) {
	var registry HypothesisRegistry
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&registry); err != nil {
		return HypothesisRegistry{}, fmt.Errorf("decode hypothesis registry: %w", err)
	}
	if err := registry.Validate(); err != nil {
		return HypothesisRegistry{}, err
	}
	return registry, nil
}

// Validate checks that every experiment dependency is registered before a run starts.
func (r HypothesisRegistry) Validate() error {
	if r.Version != HypothesisRegistryVersion {
		return fmt.Errorf("unsupported hypothesis registry version %q", r.Version)
	}
	metrics, err := uniqueIDs("metric", r.Metrics, func(v Metric) string { return v.ID })
	if err != nil {
		return err
	}
	cohorts, err := uniqueIDs("cohort", r.Cohorts, func(v Cohort) string { return v.ID })
	if err != nil {
		return err
	}
	if _, err := uniqueStrings("control", r.Controls); err != nil {
		return err
	}
	if r.Repeats < 3 {
		return fmt.Errorf("repeats must be at least 3")
	}
	arms, err := uniqueIDs("arm", r.Arms, func(v Arm) string { return v.ID })
	if err != nil {
		return err
	}
	if len(arms) != 4 || !hasAll(arms, "ARM-A", "ARM-B", "ARM-C", "ARM-D") {
		return fmt.Errorf("registry must define ARM-A through ARM-D")
	}
	hypotheses, err := uniqueIDs("hypothesis", r.Hypotheses, func(v Hypothesis) string { return v.ID })
	if err != nil {
		return err
	}
	if len(hypotheses) != 6 || !hasAll(hypotheses, "H-001", "H-002", "H-003", "H-004", "H-005", "H-006") {
		return fmt.Errorf("registry must define H-001 through H-006")
	}
	if !hasAll(metrics, r.Decision.PrimaryMetric, r.Decision.CoPrimaryMetric, r.Decision.SuccessMetric) {
		return fmt.Errorf("decision rule references an undefined metric")
	}
	if !hasAll(cohorts, r.Decision.RequiredCohorts...) || len(r.Decision.RequiredCohorts) == 0 {
		return fmt.Errorf("decision rule references an undefined cohort")
	}
	if r.Decision.MinTokenReductionPercent != 15 || r.Decision.MinCallReductionPercent != 10 || r.Decision.MaxSuccessRegressionPP != 5 {
		return fmt.Errorf("decision rule must use BL-018 thresholds")
	}
	for _, h := range r.Hypotheses {
		if h.Statement == "" || h.Ablation == "" || h.Guardrail == "" || !hasAll(metrics, h.Metric) {
			return fmt.Errorf("hypothesis %s has an undefined metric or required field", h.ID)
		}
		if !hasAll(cohorts, h.Cohorts...) || len(h.Cohorts) == 0 || !hasAll(stringsToSet(r.Controls), h.Controls...) || len(h.Controls) == 0 {
			return fmt.Errorf("hypothesis %s has an undefined cohort or control", h.ID)
		}
	}
	return nil
}

// ContentHash is the immutable identity that must be stored with each run.
func (r HypothesisRegistry) ContentHash() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("marshal hypothesis registry: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ExperimentRun pins an IC-004 experiment run to its registered hypothesis content.
type ExperimentRun struct {
	HypothesisID string `json:"hypothesis_id"`
	ArmID        string `json:"arm_id"`
	RegistryHash string `json:"registry_hash"`
}

func NewExperimentRun(r HypothesisRegistry, hypothesisID, armID string) (ExperimentRun, error) {
	hash, err := r.ContentHash()
	if err != nil {
		return ExperimentRun{}, err
	}
	run := ExperimentRun{HypothesisID: hypothesisID, ArmID: armID, RegistryHash: hash}
	return run, run.Validate(r)
}

// Validate prevents a run from being interpreted against modified registry content.
func (r ExperimentRun) Validate(registry HypothesisRegistry) error {
	hash, err := registry.ContentHash()
	if err != nil {
		return err
	}
	if r.RegistryHash != hash {
		return fmt.Errorf("experiment run registry hash does not match registered content")
	}
	if !hasAll(stringsToSet(ids(registry.Hypotheses, func(v Hypothesis) string { return v.ID })), r.HypothesisID) || !hasAll(stringsToSet(ids(registry.Arms, func(v Arm) string { return v.ID })), r.ArmID) {
		return fmt.Errorf("experiment run references an unregistered hypothesis or arm")
	}
	return nil
}

// PairedResult is one task repeated in a registered arm comparison.
type PairedResult struct {
	Cohort              string
	BaselineSuccessful  bool
	CandidateSuccessful bool
	BaselineTokens      float64
	CandidateTokens     float64
	BaselineCalls       float64
	CandidateCalls      float64
}

// Accepts implements the BL-018 decision thresholds for paired successful tasks.
func (r DecisionRule) Accepts(results []PairedResult) (bool, error) {
	byCohort := make(map[string][]PairedResult)
	for _, result := range results {
		byCohort[result.Cohort] = append(byCohort[result.Cohort], result)
	}
	for _, cohort := range r.RequiredCohorts {
		pairs := byCohort[cohort]
		if len(pairs) == 0 {
			return false, fmt.Errorf("missing paired results for cohort %s", cohort)
		}
		var baselineTokens, candidateTokens, baselineCalls, candidateCalls []float64
		var baselineSuccesses, candidateSuccesses float64
		for _, pair := range pairs {
			if pair.BaselineSuccessful {
				baselineSuccesses++
			}
			if pair.CandidateSuccessful {
				candidateSuccesses++
			}
			if pair.BaselineSuccessful && pair.CandidateSuccessful && pair.BaselineTokens > 0 && pair.BaselineCalls > 0 {
				baselineTokens = append(baselineTokens, pair.BaselineTokens)
				candidateTokens = append(candidateTokens, pair.CandidateTokens)
				baselineCalls = append(baselineCalls, pair.BaselineCalls)
				candidateCalls = append(candidateCalls, pair.CandidateCalls)
			}
		}
		if len(baselineTokens) == 0 || (baselineSuccesses-candidateSuccesses)*100/float64(len(pairs)) > r.MaxSuccessRegressionPP {
			return false, nil
		}
		if reduction(median(baselineTokens), median(candidateTokens)) < r.MinTokenReductionPercent || reduction(median(baselineCalls), median(candidateCalls)) < r.MinCallReductionPercent {
			return false, nil
		}
	}
	return true, nil
}

func reduction(baseline, candidate float64) float64 { return (baseline - candidate) * 100 / baseline }

func median(values []float64) float64 {
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 0 {
		return (values[middle-1] + values[middle]) / 2
	}
	return values[middle]
}

func uniqueIDs[T any](kind string, values []T, id func(T) string) (map[string]struct{}, error) {
	return uniqueStrings(kind, ids(values, id))
}

func ids[T any](values []T, id func(T) string) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = id(value)
	}
	return result
}

func uniqueStrings(kind string, values []string) (map[string]struct{}, error) {
	result := stringsToSet(values)
	if len(result) != len(values) || len(result) == 0 {
		return nil, fmt.Errorf("%s IDs must be non-empty and unique", kind)
	}
	for _, value := range values {
		if value == "" {
			return nil, fmt.Errorf("%s IDs must be non-empty and unique", kind)
		}
	}
	return result, nil
}

func stringsToSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func hasAll(values map[string]struct{}, required ...string) bool {
	for _, value := range required {
		if _, ok := values[value]; !ok {
			return false
		}
	}
	return true
}
