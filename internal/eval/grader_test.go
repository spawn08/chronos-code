package eval

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGraderHiddenTestFailureOverridesSavings(t *testing.T) {
	result := NewGrader().Grade(GraderInput{
		Outcome: validTaskOutcome(),
		Commands: []CommandEvidence{{
			ID:      "hidden-tests",
			Command: "go test ./... -tags=hidden",
			Passed:  false,
			Output:  "--- FAIL: TestHidden\nexpected 2, got 1\n",
		}},
		HiddenTestID: "hidden-tests",
	})

	if result.Passed {
		t.Fatal("Grade() passed despite failed hidden tests")
	}
	if !hasFinding(result.Findings, FindingHiddenTestFailed) {
		t.Fatalf("findings = %+v, want hidden-test failure", result.Findings)
	}
	if !reflect.DeepEqual(result.Evidence, []CommandEvidence{{
		ID:      "hidden-tests",
		Command: "go test ./... -tags=hidden",
		Passed:  false,
		Output:  "--- FAIL: TestHidden\nexpected 2, got 1",
	}}) {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
}

func TestGraderCompletionClaimRequiresFreshCorrelatedCommand(t *testing.T) {
	changedAt := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	result := NewGrader().Grade(GraderInput{
		Outcome: TaskOutcome{LatestChangeAt: changedAt},
		Claims: []CompletionClaim{{
			Text:       "Tests passed",
			EvidenceID: "test-command",
		}},
		Commands: []CommandEvidence{{
			ID:         "test-command",
			Command:    "go test ./...",
			Passed:     true,
			ExecutedAt: changedAt.Add(-time.Second),
		}},
	})

	if result.Passed {
		t.Fatal("Grade() passed an unsupported completion claim")
	}
	if !hasFinding(result.Findings, FindingUnsupportedClaim) {
		t.Fatalf("findings = %+v, want unsupported claim", result.Findings)
	}
}

func TestGraderReportsMissingCitationsDeterministically(t *testing.T) {
	input := GraderInput{
		Citations: []Citation{
			{Path: "z.go", Symbol: "MissingZ"},
			{Path: "a.go", Symbol: "MissingA"},
		},
		Paths:   map[string]struct{}{"a.go": {}},
		Symbols: map[string]struct{}{},
	}

	first := NewGrader().Grade(input)
	second := NewGrader().Grade(input)
	if first.Passed || second.Passed {
		t.Fatal("Grade() passed missing citations")
	}
	if !reflect.DeepEqual(first.Findings, second.Findings) {
		t.Fatalf("findings differ: %+v, %+v", first.Findings, second.Findings)
	}
	if got := []string{first.Findings[0].Message, first.Findings[1].Message, first.Findings[2].Message}; !reflect.DeepEqual(got, []string{
		"cited path \"z.go\" does not exist",
		"cited symbol \"MissingA\" does not exist",
		"cited symbol \"MissingZ\" does not exist",
	}) {
		t.Fatalf("finding messages = %v", got)
	}
}

func TestGraderRetainsConciseCommandOutput(t *testing.T) {
	result := NewGrader().Grade(GraderInput{Commands: []CommandEvidence{{
		ID:      "build",
		Command: "go test ./...",
		Passed:  true,
		Output:  strings.Repeat("x", maxEvidenceOutput+1),
	}}})

	if len(result.Evidence) != 1 || len(result.Evidence[0].Output) != maxEvidenceOutput {
		t.Fatalf("evidence = %+v, want output limited to %d bytes", result.Evidence, maxEvidenceOutput)
	}
}

func hasFinding(findings []Finding, kind FindingKind) bool {
	for _, finding := range findings {
		if finding.Kind == kind {
			return true
		}
	}
	return false
}
