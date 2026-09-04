package verification

import (
	"testing"

	"github.com/spawn08/chronos-code/internal/execution"
)

func TestDeriveIncludesConfiguredChecksForEdits(t *testing.T) {
	got := Derive(Input{
		TaskKind: "debug", ChangedPaths: []string{"internal/auth.go", "internal/auth.go"},
		BuildCommand: "go build ./...", Diagnostics: "go vet ./...",
		ImpactPaths: []string{"internal/auth_test.go"},
		TestMap:     map[string][]string{"internal/auth.go": {"go test ./internal/auth", "go test ./internal/auth"}},
		DiffCommand: "git diff --check",
	})
	if len(got) != 4 {
		t.Fatalf("obligations = %#v, want build, diagnostics, test, and diff", got)
	}
	for _, obligation := range got {
		if obligation.Status != StatusPending || len(obligation.Paths) != 2 {
			t.Fatalf("invalid derived obligation: %#v", obligation)
		}
	}
}

func TestDeriveSkipsEditObligationsForReadOnlyTasks(t *testing.T) {
	for _, kind := range []string{"explain", "architect"} {
		if got := Derive(Input{TaskKind: kind, ChangedPaths: []string{"main.go"}, BuildCommand: "go build ./..."}); len(got) != 0 {
			t.Fatalf("%s obligations = %#v, want none", kind, got)
		}
	}
}

func TestReduceRequiresAPostWriteCheck(t *testing.T) {
	obligations := Derive(Input{TaskKind: "edit", ChangedPaths: []string{"main.go"}, TestCommands: []string{"go test ./..."}})
	events := []execution.Event{
		{ID: "check-before", TaskID: "task", Sequence: 1, Type: execution.EventVerification, EvidenceID: "before", Paths: []string{"main.go"}, Detail: "go test ./...", Passed: true},
		{ID: "write", TaskID: "task", Sequence: 2, Type: execution.EventWrite, Paths: []string{"main.go"}},
	}
	got := Reduce(obligations, events)
	if got[0].Status != StatusStale {
		t.Fatalf("status after write = %q, want %q", got[0].Status, StatusStale)
	}
	if decision := Assess(ModeEnforce, true, obligations, events); decision.Allowed || !decision.Disagreement {
		t.Fatalf("enforced unsupported success = %#v", decision)
	}

	events = append(events, execution.Event{ID: "check-after", TaskID: "task", Sequence: 3, Type: execution.EventVerification, EvidenceID: "after", Paths: []string{"main.go"}, Detail: "go test ./...", Passed: true})
	if got = Reduce(obligations, events); got[0].Status != StatusSatisfied {
		t.Fatalf("status after check = %q, want %q", got[0].Status, StatusSatisfied)
	}
}

func TestFailedAndCancelledChecksBlockButRemainVisible(t *testing.T) {
	obligations := Derive(Input{TaskKind: "edit", ChangedPaths: []string{"main.go"}, BuildCommand: "go build ./..."})
	failed := []execution.Event{{ID: "failed", TaskID: "task", Sequence: 1, Type: execution.EventVerification, EvidenceID: "failed", Paths: []string{"main.go"}, Detail: "go build ./..."}}
	if got := Reduce(obligations, failed); got[0].Status != StatusFailed {
		t.Fatalf("failed status = %q", got[0].Status)
	}
	cancelled := []execution.Event{{ID: "cancelled", TaskID: "task", Sequence: 1, Type: execution.EventUncertainty, Paths: []string{"main.go"}, Detail: "cancelled:go build ./..."}}
	decision := Assess(ModeReport, true, obligations, cancelled)
	if decision.Allowed != true || !decision.Disagreement || decision.Obligations[0].Status != StatusCancelled {
		t.Fatalf("report-only cancellation = %#v", decision)
	}
}
