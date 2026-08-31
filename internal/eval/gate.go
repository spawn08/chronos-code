package eval

import (
	"encoding/json"
	"fmt"
	"os"
)

// MaxRegressionPercent is the PRD P3-006 CI gate threshold: a PR that
// increases optimized tokens-per-suite by more than this percent, relative
// to the checked-in baseline snapshot, fails the gate.
const MaxRegressionPercent = 10.0

// Baseline is the checked-in snapshot a suite run is compared against. It is
// deliberately narrow (just the two totals) so it stays diff-friendly in
// version control.
type Baseline struct {
	TotalBaseline  int `json:"total_baseline_tokens"`
	TotalOptimized int `json:"total_optimized_tokens"`
}

// LoadBaseline reads a checked-in Baseline snapshot from path. A missing file
// is not an error — it signals "no baseline yet" to CheckRegression, which
// callers use to seed the first snapshot via SaveBaseline.
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("eval: read baseline %s: %w", path, err)
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("eval: parse baseline %s: %w", path, err)
	}
	return &b, nil
}

// SaveBaseline writes s's totals to path as the new checked-in snapshot.
func SaveBaseline(path string, s Summary) error {
	b := Baseline{TotalBaseline: s.TotalBaseline, TotalOptimized: s.TotalOptimized}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: encode baseline: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("eval: write baseline %s: %w", path, err)
	}
	return nil
}

// CheckRegression fails the gate when either (a) any task's efficiency
// contract broke (FailedTasks non-empty — a functional regression, worse
// than a token regression since it means the savings machinery silently
// stopped firing), or (b) current's optimized token total increased by more
// than MaxRegressionPercent versus stored. A nil stored baseline (no
// snapshot yet) always passes — the caller is expected to seed one via
// SaveBaseline in that case.
func CheckRegression(current Summary, stored *Baseline) error {
	if len(current.FailedTasks) > 0 {
		return fmt.Errorf("eval gate: %d task(s) failed their efficiency contract: %v", len(current.FailedTasks), current.FailedTasks)
	}
	if stored == nil {
		return nil
	}
	if stored.TotalOptimized == 0 {
		return nil
	}
	delta := float64(current.TotalOptimized-stored.TotalOptimized) / float64(stored.TotalOptimized) * 100
	if delta > MaxRegressionPercent {
		return fmt.Errorf("eval gate: optimized tokens regressed %.1f%% (%d -> %d), exceeds %.0f%% threshold",
			delta, stored.TotalOptimized, current.TotalOptimized, MaxRegressionPercent)
	}
	return nil
}
