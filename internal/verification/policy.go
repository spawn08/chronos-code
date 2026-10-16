// Package verification derives completion requirements from execution evidence.
package verification

import (
	"path/filepath"
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
	KindQuiescence  Kind = "quiescence"
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
	ID            string
	Kind          Kind
	Command       string
	CommandClass  execution.CommandClass
	CommandFamily string
	CheckRoot     string
	Paths         []string
	Status        Status
}

// Decision describes whether the policy agrees with a caller's proposed
// successful completion. In report mode completion remains allowed; enforce
// mode blocks it. Disagreement is suitable for metrics in either mode.
type Decision struct {
	Allowed      bool
	Disagreement bool
	Obligations  []Obligation
}

// Derive selects required repository checks. Observed mutation, rather than
// the requested task classification, is authoritative.
func Derive(input Input) []Obligation {
	if len(input.ChangedPaths) == 0 {
		return nil
	}

	paths := unique(append(append([]string(nil), input.ChangedPaths...), input.ImpactPaths...))
	testCommands := append([]string(nil), input.TestCommands...)
	for path, commands := range input.TestMap {
		if overlap(paths, []string{path}) {
			testCommands = append(testCommands, commands...)
		}
	}
	obligations := make([]Obligation, 0, 3+len(testCommands))
	add := func(kind Kind, command string, class execution.CommandClass) {
		id := string(kind)
		if command != "" {
			id += ":" + command
		}
		obligations = append(obligations, Obligation{
			ID: id, Kind: kind, Command: command, CommandClass: class, Paths: paths, Status: StatusPending,
		})
	}
	if input.BuildCommand != "" {
		add(KindBuild, input.BuildCommand, execution.CommandBuild)
	}
	if input.Diagnostics != "" {
		add(KindDiagnostics, input.Diagnostics, execution.CommandDiagnostics)
	}
	for _, command := range unique(testCommands) {
		add(KindTest, command, execution.CommandTest)
	}
	if len(testCommands) == 0 && containsCode(paths) {
		add(KindTest, "", execution.CommandTest)
	}
	// A diff review is required only when the caller names its command: the
	// edit tool already returns each change, and a mandatory `git diff
	// --stat` forced an extra repair turn after most edits.
	if input.DiffCommand != "" {
		add(KindDiff, input.DiffCommand, execution.CommandDiff)
	}
	return obligations
}

// Reduce updates obligations from append-only ledger evidence. A later write
// to an obligation path stales an earlier successful result; a failed or
// cancelled result remains visible until a later successful check replaces it.
func Reduce(obligations []Obligation, events []execution.Event) []Obligation {
	current := clone(obligations)
	for i := range current {
		obligation := &current[i]
		statuses := make([]Status, len(obligation.Paths))
		evidence := make([]execution.Event, len(obligation.Paths))
		for j := range statuses {
			statuses[j] = StatusPending
		}
		for _, event := range events {
			for j, path := range obligation.Paths {
				switch event.Type {
				case execution.EventWrite:
					if statuses[j] == StatusSatisfied && (overlap([]string{path}, event.Paths) || execution.VerificationInputsOverlap(evidence[j], event)) {
						statuses[j] = StatusStale
					}
				case execution.EventVerification:
					if covers(event.Paths, path) && matches(*obligation, event) {
						statuses[j] = StatusFailed
						if event.TerminalState == execution.TerminalCancelled {
							statuses[j] = StatusCancelled
						}
						evidence[j] = event
						if event.Passed {
							statuses[j] = StatusSatisfied
						}
					}
				case execution.EventUncertainty:
					if overlap([]string{path}, event.Paths) && matchesDetail(*obligation, strings.TrimPrefix(event.Detail, "cancelled:")) && strings.HasPrefix(event.Detail, "cancelled:") {
						statuses[j] = StatusCancelled
					}
				}
			}
		}
		obligation.Status = StatusPending
		if len(statuses) > 0 {
			obligation.Status = StatusSatisfied
			for _, status := range []Status{StatusFailed, StatusCancelled, StatusStale, StatusPending} {
				for _, pathStatus := range statuses {
					if pathStatus == status {
						obligation.Status = status
						break
					}
				}
				if obligation.Status != StatusSatisfied {
					break
				}
			}
		}
	}
	return current
}

// Verification coverage must contain an input; overlap alone can cover a sibling.
func covers(paths []string, target string) bool {
	for _, path := range paths {
		if path == "." || path == target || strings.HasPrefix(target, strings.TrimSuffix(path, "/")+"/") {
			return true
		}
	}
	return false
}

func matchesDetail(obligation Obligation, detail string) bool {
	return detail == obligation.ID || obligation.Command != "" && detail == obligation.Command
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

func matches(obligation Obligation, event execution.Event) bool {
	if obligation.CommandFamily != "" {
		return event.CommandFamily == obligation.CommandFamily && event.CheckRoot == obligation.CheckRoot && event.CommandClass == obligation.CommandClass
	}
	if obligation.Kind == KindQuiescence {
		return false
	}
	if obligation.Command != "" {
		if obligation.Kind == KindTest {
			command := event.Command
			command = strings.TrimPrefix(strings.TrimPrefix(command, "python3 -m "), "python -m ")
			switch obligation.Command {
			case "go test", "pytest", "cargo test", "npm test":
				if command == obligation.Command || strings.HasPrefix(command, obligation.Command+" ") {
					return true
				}
			}
		}
		return event.Command == obligation.Command || event.Detail == obligation.ID || event.Detail == obligation.Command
	}
	return event.CommandClass == obligation.CommandClass
}

func containsCode(paths []string) bool {
	for _, path := range paths {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".c", ".cc", ".cpp", ".cs", ".go", ".java", ".js", ".jsx", ".kt", ".kts", ".php", ".py", ".rb", ".rs", ".swift", ".ts", ".tsx", ".css", ".scss", ".sass", ".less", ".html", ".htm", ".vue", ".svelte", ".proto", ".graphql", ".gql":
			return true
		}
	}
	return false
}

func overlap(left, right []string) bool {
	for _, l := range left {
		for _, r := range right {
			if l == "." || r == "." || l == r || strings.HasPrefix(l, strings.TrimSuffix(r, "/")+"/") || strings.HasPrefix(r, strings.TrimSuffix(l, "/")+"/") {
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
