package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spawn08/chronos-code/internal/eval"
)

// defaultBaselinePath is the checked-in eval suite snapshot the CI gate
// compares against (PRD P3-006).
const defaultBaselinePath = "benchmark/eval/baseline.json"

// runEval implements `chronos-code eval run`. It needs no config, API key, or
// storage backend — the suite is fully offline and deterministic (see
// internal/eval's package doc) — so it deliberately does not go through
// loadAndBuild.
func runEval() error {
	if len(os.Args) < 3 || os.Args[2] != "run" {
		return fmt.Errorf("usage: chronos-code eval run [--update-baseline] [--baseline <path>] [--md <path>]")
	}
	rest := os.Args[3:]
	baselinePath := flagValue(rest, "baseline", defaultBaselinePath)
	updateBaseline := hasFlag(rest, "update-baseline")
	mdPath := flagValue(rest, "md", "")

	ctx := context.Background()
	results, err := eval.RunAll(ctx)
	if err != nil {
		return fmt.Errorf("run eval suite: %w", err)
	}
	summary, err := eval.Summarize(results)
	if err != nil {
		return fmt.Errorf("summarize eval suite: %w", err)
	}

	report := summary.RenderMarkdown()
	fmt.Print(report)
	if mdPath != "" {
		if err := os.WriteFile(mdPath, []byte(report), 0o644); err != nil {
			return fmt.Errorf("write markdown report to %s: %w", mdPath, err)
		}
	}

	if updateBaseline {
		if err := eval.SaveBaseline(baselinePath, summary); err != nil {
			return fmt.Errorf("update baseline: %w", err)
		}
		fmt.Printf("\nbaseline updated: %s\n", baselinePath)
		return nil
	}

	stored, err := eval.LoadBaseline(baselinePath)
	if err != nil {
		return fmt.Errorf("load baseline: %w", err)
	}
	if stored == nil {
		fmt.Printf("\nno baseline at %s yet — run with --update-baseline to create one\n", baselinePath)
		return nil
	}
	if err := eval.CheckRegression(summary, stored); err != nil {
		return err
	}
	fmt.Printf("\neval gate passed (baseline: %s)\n", baselinePath)
	return nil
}
