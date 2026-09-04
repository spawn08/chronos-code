package eval

import (
	"sort"
	"strings"
	"time"
)

const maxEvidenceOutput = 512

// FindingKind classifies a deterministic reason a task did not verify.
type FindingKind string

const (
	FindingHiddenTestFailed FindingKind = "hidden_test_failed"
	FindingUnsupportedClaim FindingKind = "unsupported_claim"
	FindingMissingPath      FindingKind = "missing_path"
	FindingMissingSymbol    FindingKind = "missing_symbol"
)

// Finding describes one failed grading check.
type Finding struct {
	Kind    FindingKind
	Message string
}

// CommandEvidence is a command and its concise retained output.
type CommandEvidence struct {
	ID         string
	Command    string
	Passed     bool
	Output     string
	ExecutedAt time.Time
}

// CompletionClaim is a final-answer completion statement tied to a command.
type CompletionClaim struct {
	Text       string
	EvidenceID string
}

// Citation identifies a path or symbol referenced by a final answer.
type Citation struct {
	Path   string
	Symbol string
}

// GraderInput contains the immutable records inspected by deterministic checks.
type GraderInput struct {
	Outcome      TaskOutcome
	Commands     []CommandEvidence
	HiddenTestID string
	Claims       []CompletionClaim
	Citations    []Citation
	Paths        map[string]struct{}
	Symbols      map[string]struct{}
}

// GradeResult is the binary grade with retained command evidence and findings.
type GradeResult struct {
	Passed   bool
	Findings []Finding
	Evidence []CommandEvidence
}

// GraderCheck is one independently composable deterministic grading check.
type GraderCheck func(GraderInput) []Finding

// Grader applies checks in order. A finding always makes the grade unsuccessful.
type Grader struct {
	Checks []GraderCheck
}

// NewGrader returns the standard deterministic correctness grader.
func NewGrader() Grader {
	return Grader{Checks: []GraderCheck{checkHiddenTests, checkClaims, checkCitations}}
}

// Grade evaluates all checks, retaining commands and bounded command output.
func (g Grader) Grade(input GraderInput) GradeResult {
	result := GradeResult{Passed: true, Evidence: conciseEvidence(input.Commands)}
	for _, check := range g.Checks {
		result.Findings = append(result.Findings, check(input)...)
	}
	result.Passed = len(result.Findings) == 0
	return result
}

func checkHiddenTests(input GraderInput) []Finding {
	if input.HiddenTestID == "" {
		return nil
	}
	for _, command := range input.Commands {
		if command.ID == input.HiddenTestID {
			if command.Passed {
				return nil
			}
			return []Finding{{Kind: FindingHiddenTestFailed, Message: "hidden tests failed"}}
		}
	}
	return []Finding{{Kind: FindingHiddenTestFailed, Message: "hidden test command evidence is missing"}}
}

func checkClaims(input GraderInput) []Finding {
	commands := make(map[string]CommandEvidence, len(input.Commands))
	for _, command := range input.Commands {
		commands[command.ID] = command
	}

	findings := make([]Finding, 0)
	for _, claim := range input.Claims {
		command, ok := commands[claim.EvidenceID]
		if claim.EvidenceID == "" || !ok || command.Command == "" || !command.Passed || command.ExecutedAt.Before(input.Outcome.LatestChangeAt) {
			findings = append(findings, Finding{Kind: FindingUnsupportedClaim, Message: "completion claim lacks fresh successful command evidence: " + claim.Text})
		}
	}
	return findings
}

func checkCitations(input GraderInput) []Finding {
	missingPaths := make([]string, 0)
	missingSymbols := make([]string, 0)
	for _, citation := range input.Citations {
		if citation.Path != "" {
			if _, ok := input.Paths[citation.Path]; !ok {
				missingPaths = append(missingPaths, citation.Path)
			}
		}
		if citation.Symbol != "" {
			if _, ok := input.Symbols[citation.Symbol]; !ok {
				missingSymbols = append(missingSymbols, citation.Symbol)
			}
		}
	}
	sort.Strings(missingPaths)
	sort.Strings(missingSymbols)

	findings := make([]Finding, 0, len(missingPaths)+len(missingSymbols))
	for _, path := range missingPaths {
		findings = append(findings, Finding{Kind: FindingMissingPath, Message: "cited path \"" + path + "\" does not exist"})
	}
	for _, symbol := range missingSymbols {
		findings = append(findings, Finding{Kind: FindingMissingSymbol, Message: "cited symbol \"" + symbol + "\" does not exist"})
	}
	return findings
}

func conciseEvidence(commands []CommandEvidence) []CommandEvidence {
	evidence := make([]CommandEvidence, len(commands))
	copy(evidence, commands)
	for i := range evidence {
		evidence[i].Output = strings.TrimSpace(evidence[i].Output)
		if len(evidence[i].Output) > maxEvidenceOutput {
			evidence[i].Output = evidence[i].Output[:maxEvidenceOutput]
		}
	}
	return evidence
}
