// Package verification derives completion requirements from execution evidence.
package verification

import (
	"sort"
	"strings"

	"github.com/spawn08/chronos-code/internal/execution"
)

type Mode string

const (
	ModeReport  Mode = "report"
	ModeEnforce Mode = "enforce"
)

type Kind string

const (
	KindBuild       Kind = "build"
	KindDiagnostics Kind = "diagnostics"
	KindTest        Kind = "test"
	KindDiff        Kind = "diff"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusSatisfied Status = "satisfied"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
	StatusStale     Status = "stale"
)

// Input is the deterministic repository information used to select checks.
// TaskKind follows the router's string values; only edit, refactor, and debug
// tasks acquire edit-specific obligations.
type Input struct {
	TaskKind     string
	ChangedPaths []string
	ImpactPaths  []string
	BuildCommand string
	Diagnostics  string
	TestCommands []string
	// TestMap maps an affected path to its targeted test commands.
	TestMap     map[string][]string
	DiffCommand string
}

// Obligation identifies one check and its current evidence-derived state.
type Obligation struct {
	ID      string
	Kind    Kind
	Command string
	Paths   []string
	Status  Status
}

// Decision describes whether the policy agrees with a caller's proposed
// successful completion. In report mode completion remains allowed; enforce
// mode blocks it. Disagreement is suitable for metrics in either mode.
type Decision struct {
	Allowed      bool
	Disagreement bool
	Obligations  []Obligation
}

// Derive selects required repository checks. Read-only tasks and tasks with
// no writes have no edit-specific verification requirements.
func Derive(input Input) []Obligation {
	if !isEditTask(input.TaskKind) || len(input.ChangedPaths) == 0 {
		return nil
	}

	paths := unique(append(append([]string(nil), input.ChangedPaths...), input.ImpactPaths...))
	testCommands := append([]string(nil), input.TestCommands...)
	for _, commands := range input.TestMap {
		testCommands = append(testCommands, commands...)
	}
	obligations := make([]Obligation, 0, 3+len(testCommands))
	add := func(kind Kind, command string) {
		if command == "" {
			return
		}
		obligations = append(obligations, Obligation{
			ID: string(kind) + ":" + command, Kind: kind, Command: command, Paths: paths, Status: StatusPending,
		})
	}
	add(KindBuild, input.BuildCommand)
	add(KindDiagnostics, input.Diagnostics)
	for _, command := range unique(testCommands) {
		add(KindTest, command)
	}
	add(KindDiff, input.DiffCommand)
	return obligations
}

// Reduce updates obligations from append-only ledger evidence. A later write
// to an obligation path stales an earlier successful result; a failed or
// cancelled result remains visible until a later successful check replaces it.
func Reduce(obligations []Obligation, events []execution.Event) []Obligation {
	current := clone(obligations)
	for i := range current {
		current[i].Status = StatusPending
	}
	for _, event := range events {
		for i := range current {
			obligation := &current[i]
			if !overlap(obligation.Paths, event.Paths) {
				continue
			}
			switch event.Type {
			case execution.EventWrite:
				if obligation.Status == StatusSatisfied {
					obligation.Status = StatusStale
				}
			case execution.EventVerification:
				if !matches(*obligation, event.Detail) {
					continue
				}
				if event.Passed {
					obligation.Status = StatusSatisfied
				} else {
					obligation.Status = StatusFailed
				}
			case execution.EventUncertainty:
				if matches(*obligation, strings.TrimPrefix(event.Detail, "cancelled:")) && strings.HasPrefix(event.Detail, "cancelled:") {
					obligation.Status = StatusCancelled
				}
			}
		}
	}
	return current
}

// Assess derives the current obligation result and records a measurable
// disagreement whenever a proposed success lacks current supporting evidence.
func Assess(mode Mode, proposedSuccess bool, obligations []Obligation, events []execution.Event) Decision {
	current := Reduce(obligations, events)
	supported := true
	for _, obligation := range current {
		if obligation.Status != StatusSatisfied {
			supported = false
			break
		}
	}
	disagreement := proposedSuccess && !supported
	return Decision{Allowed: !disagreement || mode == ModeReport, Disagreement: disagreement, Obligations: current}
}

func isEditTask(kind string) bool {
	return kind == "edit" || kind == "refactor" || kind == "debug"
}

func matches(obligation Obligation, detail string) bool {
	return detail == obligation.ID || detail == obligation.Command
}

func overlap(left, right []string) bool {
	for _, l := range left {
		for _, r := range right {
			if l == r {
				return true
			}
		}
	}
	return false
}

func unique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func clone(obligations []Obligation) []Obligation {
	cloned := make([]Obligation, len(obligations))
	copy(cloned, obligations)
	for i := range cloned {
		cloned[i].Paths = append([]string(nil), cloned[i].Paths...)
	}
	return cloned
}
