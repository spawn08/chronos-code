package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spawn08/chronos-code/internal/eval"
)

// defaultBaselinePath is the checked-in eval suite snapshot the CI gate
// compares against (PRD P3-006).
const defaultBaselinePath = "benchmark/eval/baseline.json"

const (
	defaultPPDTasksPath      = "benchmark/ppd/tasks.yaml"
	defaultPPDHypothesesPath = "benchmark/ppd/hypotheses.yaml"
	defaultPPDResultsPath    = "benchmark/ppd/results.json"
	defaultPPDBaselinePath   = "benchmark/ppd/baseline.json"
)

// runEval implements `chronos-code eval run`. It needs no config, API key, or
// storage backend — the suite is fully offline and deterministic (see
// internal/eval's package doc) — so it deliberately does not go through
// loadAndBuild.
func runEval() error {
	if len(os.Args) >= 3 && os.Args[2] == "ppd" {
		if len(os.Args) >= 4 && os.Args[3] == "--report" {
			rest := os.Args[4:]
			if err := validatePPDReportFlags(rest); err != nil {
				return err
			}
			report, err := eval.BuildPPDReport(
				flagValue(rest, "tasks", defaultPPDTasksPath),
				flagValue(rest, "hypotheses", defaultPPDHypothesesPath),
				flagValue(rest, "results", defaultPPDResultsPath),
				flagValue(rest, "baseline", defaultPPDBaselinePath),
			)
			if report != "" {
				fmt.Print(report)
			}
			return err
		}
		if len(os.Args) != 4 || os.Args[3] != "--validate-only" {
			return fmt.Errorf("usage: chronos-code eval ppd --validate-only | chronos-code eval ppd --report [--tasks <path>] [--hypotheses <path>] [--results <path>] [--baseline <path>]")
		}
		if err := eval.ValidatePPDBenchmark(defaultPPDTasksPath, defaultPPDResultsPath); err != nil {
			return fmt.Errorf("validate PPD benchmark: %w", err)
		}
		status, reason, err := eval.PPDBenchmarkEvidenceStatus(defaultPPDResultsPath)
		if err != nil {
			return fmt.Errorf("read PPD evidence status: %w", err)
		}
		fmt.Printf("PPD benchmark registration is structurally valid; executed evidence status: %s", status)
		if reason != "" {
			fmt.Printf(" (%s)", reason)
		}
		fmt.Println()
		return nil
	}
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

func validatePPDReportFlags(args []string) error {
	allowed := map[string]bool{"--tasks": true, "--hypotheses": true, "--results": true, "--baseline": true}
	for i := 0; i < len(args); i++ {
		name := args[i]
		if equal := strings.IndexByte(name, '='); equal >= 0 {
			if !allowed[name[:equal]] || equal == len(name)-1 {
				return fmt.Errorf("invalid PPD report flag %q", name)
			}
			continue
		}
		if !allowed[name] || i+1 == len(args) || strings.HasPrefix(args[i+1], "--") {
			return fmt.Errorf("invalid PPD report flag %q", name)
		}
		i++
	}
	return nil
}
