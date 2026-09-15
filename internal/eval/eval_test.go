package eval

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage/adapters/memory"

	"github.com/spawn08/chronos-code/internal/toolcompress"
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

func sourceEvidence(content string, start, end int) map[string]any {
	return map[string]any{"path": "/workspace/source.go", "content": content,
		"start_line": start, "end_line": end, "total_lines": nil, "truncated": false}
}

func TestValidateStepRequiresRequestedSource(t *testing.T) {
	source := "package fixture\nfunc First() int { return 1 }\nfunc Second() int { return 2 }\n"
	files := map[string]string{"source.go": source}
	fullStep := Step{Tool: "file_read", Args: map[string]any{"path": "source.go", "outline_only": false}}
	rangeStep := Step{Tool: "file_read", Args: map[string]any{"path": "source.go", "start_line": 3, "end_line": 3}}
	full := sourceEvidence(source, 1, 4)
	ranged := sourceEvidence("func Second() int { return 2 }", 3, 3)
	for _, tc := range []struct {
		name string
		step Step
		out  map[string]any
		pass bool
	}{
		{"full", fullStep, full, true},
		{"range", rangeStep, ranged, true},
		{"repeat full", fullStep, full, true},
		{"wrong line", rangeStep, sourceEvidence("func First() int { return 1 }", 3, 3), false},
		{"full instead of range", rangeStep, full, false},
		{"range instead of full", fullStep, ranged, false},
		{"unchanged", fullStep, map[string]any{"path": "/workspace/source.go", "unchanged": true}, false},
		{"outline instead of full", fullStep, map[string]any{"path": "/workspace/source.go", "outline": true, "declarations": []string{"func First() int"}}, false},
		{"outline instead of range", rangeStep, map[string]any{"path": "/workspace/source.go", "outline": true, "declarations": []string{"func Second() int"}}, false},
		{"missing content", fullStep, map[string]any{"path": "/workspace/source.go"}, false},
		{"truncated content", fullStep, sourceEvidence(source[:20], 1, 4), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both logical and delivered values are identically wrong in the
			// negative cases. Equality alone must never establish correctness.
			violations := ValidateStep(context.Background(), tc.step, files, tc.out, tc.out, nil)
			if (len(violations) == 0) != tc.pass {
				t.Fatalf("pass=%t, violations=%v", tc.pass, violations)
			}
		})
	}
}

func TestValidateStepRequiresCompleteOutline(t *testing.T) {
	source := genGoFile("fixture", 15, "Operation")
	var declarations []string
	for i := 0; i < 15; i++ {
		declarations = append(declarations, fmt.Sprintf("func Operation%d(x int) int", i))
	}
	step := Step{Tool: "file_read", Args: map[string]any{"path": "source.go"}}
	files := map[string]string{"source.go": source}
	for _, tc := range []struct {
		name  string
		decls []string
		pass  bool
	}{
		{"complete", declarations, true},
		{"empty", []string{}, false},
		{"omitted function", declarations[:14], false},
		{"wrong signature", append(append([]string{}, declarations[:14]...), "func Operation14(x string) int"), false},
		{"body instead of signature", append(append([]string{}, declarations[:14]...), "func Operation14(x int) int { return x }"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]any{"path": "/workspace/source.go", "outline": true, "declarations": tc.decls}
			violations := ValidateStep(context.Background(), step, files, out, out, nil)
			if (len(violations) == 0) != tc.pass {
				t.Fatalf("pass=%t, violations=%v", tc.pass, violations)
			}
		})
	}
}

func compressedEvidence(t *testing.T, logical any) (*tool.Registry, map[string]any) {
	t.Helper()
	a := &agent.Agent{ID: "evidence-test", Tools: tool.NewRegistry(), Storage: memory.New(), Model: stubProvider{evalModelName}}
	a.Tools.Register(&tool.Definition{Name: "file_read", Permission: tool.PermAllow,
		Handler: func(context.Context, map[string]any) (any, error) { return logical, nil }})
	toolcompress.Wrap(a, compressionThresholdTokens)
	out, err := a.Tools.Execute(context.Background(), "file_read", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["compressed"] != true {
		t.Fatal("fixture did not exercise compression")
	}
	return a.Tools, m
}

func TestValidateStepRetrievesRequestedRangeEvidence(t *testing.T) {
	// The requested body extends past a preview and a default retrieval chunk.
	// The final sentinel is available only by following continuation offsets.
	body := strings.Repeat("requested evidence λ\n", 5000) + "END_OF_REQUESTED_RANGE"
	source := "package fixture\n" + body + "\noutside range"
	step := Step{Tool: "file_read", Args: map[string]any{"path": "source.go", "start_line": 2, "end_line": 5002}}
	logical := sourceEvidence(body, 2, 5002)
	reg, delivered := compressedEvidence(t, logical)
	if strings.Contains(delivered["preview"].(string), "END_OF_REQUESTED_RANGE") {
		t.Fatal("sentinel unexpectedly fits in preview")
	}
	reader, _ := reg.Get(toolcompress.ReadStoredResultTool)
	original := reader.Handler
	calls := 0
	reader.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		calls++
		return original(ctx, args)
	}
	if v := ValidateStep(context.Background(), step, map[string]string{"source.go": source}, logical, delivered, reg); len(v) != 0 {
		t.Fatalf("recoverable evidence rejected: %v", v)
	}
	if calls < 2 {
		t.Fatalf("expected multi-chunk public retrieval, got %d calls", calls)
	}

	t.Run("uncompressed oversized logical result", func(t *testing.T) {
		if v := ValidateStep(context.Background(), step, map[string]string{"source.go": source}, logical, logical, reg); len(v) == 0 {
			t.Fatal("missing compression accepted")
		}
	})
	t.Run("unknown storage key", func(t *testing.T) {
		bad := cloneArgs(delivered)
		bad["storage_key"] = "does-not-exist"
		if v := ValidateStep(context.Background(), step, map[string]string{"source.go": source}, logical, bad, reg); len(v) == 0 {
			t.Fatal("unreadable reference accepted")
		}
	})
	t.Run("oversized compressed envelope", func(t *testing.T) {
		bad := cloneArgs(delivered)
		bad["preview"] = strings.Repeat("unbounded preview ", 1000)
		if v := ValidateStep(context.Background(), step, map[string]string{"source.go": source}, logical, bad, reg); len(v) == 0 {
			t.Fatal("oversized compressed envelope accepted")
		}
	})
	t.Run("wrong stored evidence", func(t *testing.T) {
		wrong := sourceEvidence(strings.ReplaceAll(body, "requested", "incorrect"), 2, 5002)
		wrongReg, wrongDelivered := compressedEvidence(t, wrong)
		if v := ValidateStep(context.Background(), step, map[string]string{"source.go": source}, logical, wrongDelivered, wrongReg); len(v) == 0 {
			t.Fatal("reference to different evidence accepted")
		}
	})
	t.Run("broken continuation", func(t *testing.T) {
		reader.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			out, err := original(ctx, args)
			if err == nil {
				out.(map[string]any)["next_offset"] = 0
			}
			return out, err
		}
		defer func() { reader.Handler = original }()
		if v := ValidateStep(context.Background(), step, map[string]string{"source.go": source}, logical, delivered, reg); len(v) == 0 {
			t.Fatal("non-progressing chunk accepted")
		}
	})
}

func TestValidateStepUsesLogicalCompressionThreshold(t *testing.T) {
	source := genGoFile("fixture", 25, "Operation")
	step := Step{Tool: "file_read", Args: map[string]any{"path": "source.go", "start_line": 1, "end_line": 2}}
	logical := sourceEvidence("package fixture\n", 1, 2)
	counter := model.NewTokenCounter(evalModelName)
	if jsonTokens(counter, map[string]any{"content": source}, workspacePlaceholder) <= compressionThresholdTokens {
		t.Fatal("raw baseline must exceed compression threshold")
	}
	if v := ValidateStep(context.Background(), step, map[string]string{"source.go": source}, logical, logical, nil); len(v) != 0 {
		t.Fatalf("short logical range incorrectly required compression: %v", v)
	}
}

func TestValidateStepRejectsMissingGrepMatches(t *testing.T) {
	step := Step{Tool: "file_grep", Args: map[string]any{"path": "source.go", "pattern": "target"}}
	files := map[string]string{"source.go": "target one\nother\ntarget two"}
	out := map[string]any{"path": "/workspace/source.go", "pattern": "target", "recursive": false, "truncated": false,
		"matches": []map[string]any{{"line_number": 1, "content": "target one"}}}
	if v := ValidateStep(context.Background(), step, files, out, out, nil); len(v) == 0 {
		t.Fatal("incomplete matching evidence accepted")
	}
}

func TestRunTaskEvidencePipelineAndDeterminism(t *testing.T) {
	rt, tiers, err := buildRouter()
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: "pipeline-regression", Difficulty: DifficultyEasy,
		Files: map[string]string{"source.go": genGoFile("fixture", 25, "Operation")},
		Steps: []Step{
			{Tool: "file_read", Args: map[string]any{"path": "source.go"}},
			{Tool: "file_read", Args: map[string]any{"path": "source.go", "start_line": 1, "end_line": 2}},
			{Tool: "file_read", Args: map[string]any{"path": "source.go", "start_line": 4, "end_line": 20}},
			{Tool: "file_read", Args: map[string]any{"path": "source.go", "outline_only": false}},
			{Tool: "file_read", Args: map[string]any{"path": "source.go", "outline_only": false}},
			{Tool: "file_grep", Args: map[string]any{"path": "source.go", "pattern": "Operation"}},
		}}
	a, err := RunTask(context.Background(), task, rt, tiers)
	if err != nil || !a.Success() {
		t.Fatalf("first replay: %+v, %v", a, err)
	}
	b, err := RunTask(context.Background(), task, rt, tiers)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("different temp workspace changed results: %+v vs %+v, err=%v", a, b, err)
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
