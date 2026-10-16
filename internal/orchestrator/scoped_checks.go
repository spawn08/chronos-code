package orchestrator

import (
	"fmt"
	"sort"

	"github.com/spawn08/chronos-code/internal/checks"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/verification"
)

func scopedCheckObligations(runtime *taskRuntime, taskKind string, changed []string) ([]verification.Obligation, error) {
	if runtime == nil {
		return verification.Derive(verification.Input{TaskKind: taskKind, ChangedPaths: changed}), nil
	}
	seen := make(map[string]bool, len(changed))
	for _, path := range changed {
		seen[path] = true
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	selected, err := checks.Select(runtime.observationContext(), runtime.workspaceRoot, paths)
	if err != nil {
		return nil, fmt.Errorf("select affected repository checks: %w", err)
	}
	var obligations []verification.Obligation
	for _, check := range selected {
		kind := verification.KindTest
		switch check.Class {
		case execution.CommandBuild:
			kind = verification.KindBuild
		case execution.CommandDiagnostics:
			kind = verification.KindDiagnostics
		}
		obligations = append(obligations, verification.Obligation{
			ID: check.Family + ":" + check.Root, Kind: kind, Command: check.Command,
			CommandClass: check.Class, CommandFamily: check.Family, CheckRoot: check.Root,
			Paths: check.Paths, Status: verification.StatusPending,
		})
		for _, path := range check.Paths {
			delete(seen, path)
		}
	}
	var uncovered []string
	for _, path := range paths {
		if seen[path] {
			uncovered = append(uncovered, path)
		}
	}
	obligations = append(obligations, verification.Derive(verification.Input{TaskKind: taskKind, ChangedPaths: uncovered})...)
	return obligations, nil
}
