package eval

import (
	"context"
	"testing"
)

func TestCorpusSize(t *testing.T) {
	corpus := Corpus()
	if len(corpus) < 20 || len(corpus) > 30 {
		t.Fatalf("corpus has %d tasks, want 20-30 per PRD P3-006", len(corpus))
	}

	categories := make(map[Category]bool)
	difficulties := make(map[Difficulty]bool)
	ids := make(map[string]bool)
	for _, task := range corpus {
		categories[task.Category] = true
		difficulties[task.Difficulty] = true
		if ids[task.ID] {
			t.Fatalf("duplicate task ID %q", task.ID)
		}
		ids[task.ID] = true
		if len(task.Steps) == 0 {
			t.Fatalf("task %q has no steps", task.ID)
		}
		if len(task.Files) == 0 {
			t.Fatalf("task %q has no fixture files", task.ID)
		}
	}
	for _, c := range []Category{CategoryBugfix, CategoryFeature, CategoryRefactor} {
		if !categories[c] {
			t.Errorf("corpus missing category %q", c)
		}
	}
	for _, d := range []Difficulty{DifficultyEasy, DifficultyMedium, DifficultyHard} {
		if !difficulties[d] {
			t.Errorf("corpus missing difficulty %q", d)
		}
	}
}

func TestRunAll(t *testing.T) {
	results, err := RunAll(context.Background())
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if len(results) != len(Corpus()) {
		t.Fatalf("got %d results, want %d", len(results), len(Corpus()))
	}

	for _, r := range results {
		if !r.Success() {
			t.Errorf("task %s failed its efficiency contract: %v", r.TaskID, r.Violations)
		}
		if r.OptimizedTokens > r.BaselineTokens {
			t.Errorf("task %s: optimized (%d) exceeds baseline (%d)", r.TaskID, r.OptimizedTokens, r.BaselineTokens)
		}
	}

	summary, err := Summarize(results)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if summary.SavingsRatio() <= 0 {
		t.Errorf("expected positive aggregate savings, got %.4f", summary.SavingsRatio())
	}
	if summary.SystemPromptTokens <= 0 {
		t.Errorf("expected a positive system prompt token count, got %d", summary.SystemPromptTokens)
	}
	t.Logf("aggregate savings: %.1f%% (%d -> %d tokens), system prompt: %d tokens, T1-routed: %d/%d",
		summary.SavingsRatio()*100, summary.TotalBaseline, summary.TotalOptimized, summary.SystemPromptTokens, summary.RoutedT1Count, len(results))
}

func TestCheckRegressionNoBaseline(t *testing.T) {
	summary := Summary{TotalOptimized: 1000}
	if err := CheckRegression(summary, nil); err != nil {
		t.Fatalf("expected nil baseline to pass, got %v", err)
	}
}

func TestCheckRegressionWithinThreshold(t *testing.T) {
	stored := &Baseline{TotalBaseline: 5000, TotalOptimized: 1000}
	summary := Summary{TotalOptimized: 1090} // +9%, under the 10% threshold
	if err := CheckRegression(summary, stored); err != nil {
		t.Fatalf("expected +9%% to pass, got %v", err)
	}
}

func TestCheckRegressionExceedsThreshold(t *testing.T) {
	stored := &Baseline{TotalBaseline: 5000, TotalOptimized: 1000}
	summary := Summary{TotalOptimized: 1200} // +20%, over the 10% threshold
	if err := CheckRegression(summary, stored); err == nil {
		t.Fatal("expected +20% to fail the gate")
	}
}

func TestCheckRegressionFunctionalFailureAlwaysFails(t *testing.T) {
	stored := &Baseline{TotalBaseline: 5000, TotalOptimized: 1000}
	summary := Summary{TotalOptimized: 900, FailedTasks: []string{"bugfix-easy-1"}} // fewer tokens, but a contract broke
	if err := CheckRegression(summary, stored); err == nil {
		t.Fatal("expected a functional contract failure to fail the gate regardless of token delta")
	}
}
