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

func TestDeriveUsesObservedMutationInsteadOfRequestedTaskKind(t *testing.T) {
	for _, kind := range []string{"explain", "architect"} {
		if got := Derive(Input{TaskKind: kind}); len(got) != 0 {
			t.Fatalf("%s without mutation obligations = %#v, want none", kind, got)
		}
		got := Derive(Input{TaskKind: kind, ChangedPaths: []string{"main.go"}})
		if len(got) != 1 || got[0].Kind != KindTest {
			t.Fatalf("%s mutated obligations = %#v, want only the generic test", kind, got)
		}
	}
}

func TestReduceMatchesGenericObligationByCommandClass(t *testing.T) {
	obligations := Derive(Input{ChangedPaths: []string{"main.go"}})
	exitCode := 0
	events := []execution.Event{
		{ID: "test", TaskID: "task", Sequence: 1, Type: execution.EventVerification, EvidenceID: "test", Paths: []string{"main.go"}, CommandClass: execution.CommandTest, ExitCode: &exitCode, TerminalState: execution.TerminalExited, Passed: true},
		{ID: "diff", TaskID: "task", Sequence: 2, Type: execution.EventVerification, EvidenceID: "diff", Paths: []string{"main.go"}, CommandClass: execution.CommandDiff, ExitCode: &exitCode, TerminalState: execution.TerminalExited, Passed: true},
	}
	decision := Assess(ModeEnforce, true, obligations, events)
	if !decision.Allowed || decision.Disagreement {
		t.Fatalf("class-matched decision = %#v", decision)
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

func TestReduceRequiresCoverageOfEveryInput(t *testing.T) {
	for _, test := range []struct {
		name     string
		paths    []string
		coverage [][]string
		want     Status
	}{
		{name: "one of two inputs", paths: []string{"pkg/a.go", "pkg/b.go"}, coverage: [][]string{{"pkg/a.go"}}, want: StatusPending},
		{name: "separate checks cover all inputs", paths: []string{"pkg/a.go", "pkg/b.go"}, coverage: [][]string{{"pkg/a.go"}, {"pkg/b.go"}}, want: StatusSatisfied},
		{name: "parent covers all inputs", paths: []string{"pkg/a.go", "pkg/b.go"}, coverage: [][]string{{"pkg"}}, want: StatusSatisfied},
		{name: "root covers all inputs", paths: []string{"pkg/a.go", "other/b.go"}, coverage: [][]string{{"."}}, want: StatusSatisfied},
		{name: "sibling is not coverage", paths: []string{"pkg/a.go"}, coverage: [][]string{{"pkg/b.go"}}, want: StatusPending},
		{name: "prefix is not ancestor", paths: []string{"pkg-other/a.go"}, coverage: [][]string{{"pkg"}}, want: StatusPending},
		{name: "child does not cover directory input", paths: []string{"pkg"}, coverage: [][]string{{"pkg/a.go"}}, want: StatusPending},
	} {
		t.Run(test.name, func(t *testing.T) {
			obligations := []Obligation{{ID: "test", Kind: KindTest, CommandClass: execution.CommandTest, Paths: test.paths, Status: StatusPending}}
			var events []execution.Event
			for _, coverage := range test.coverage {
				events = append(events, execution.Event{Type: execution.EventVerification, Paths: coverage, CommandClass: execution.CommandTest, Passed: true})
			}
			if got := Reduce(obligations, events); got[0].Status != test.want {
				t.Fatalf("Reduce() status = %q, want %q", got[0].Status, test.want)
			}
			if obligations[0].Status != StatusPending {
				t.Fatal("Reduce mutated its input obligation")
			}
			decision := Assess(ModeEnforce, true, obligations, events)
			if decision.Allowed != (test.want == StatusSatisfied) || decision.Disagreement != (test.want != StatusSatisfied) {
				t.Fatalf("coverage decision = %#v", decision)
			}
		})
	}
}

func TestReduceRejectsWrongCommandFamilyRootOrClass(t *testing.T) {
	obligations := []Obligation{{
		ID: "test:go test ./...", Kind: KindTest, Command: "go test ./...",
		CommandClass: execution.CommandTest, CommandFamily: "go test", CheckRoot: "services/api",
		Paths: []string{"services/api/main.go"}, Status: StatusPending,
	}}
	for _, test := range []struct {
		name, family, root string
		class              execution.CommandClass
		want               Status
	}{
		{name: "matching identity", family: "go test", root: "services/api", class: execution.CommandTest, want: StatusSatisfied},
		{name: "wrong family", family: "pytest", root: "services/api", class: execution.CommandTest, want: StatusPending},
		{name: "missing family", root: "services/api", class: execution.CommandTest, want: StatusPending},
		{name: "sibling root", family: "go test", root: "services/web", class: execution.CommandTest, want: StatusPending},
		{name: "parent root", family: "go test", root: ".", class: execution.CommandTest, want: StatusPending},
		{name: "missing root", family: "go test", class: execution.CommandTest, want: StatusPending},
		{name: "wrong class", family: "go test", root: "services/api", class: execution.CommandBuild, want: StatusPending},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := execution.Event{
				Type: execution.EventVerification, Paths: []string{"."}, Passed: true,
				Command: obligations[0].Command, Detail: obligations[0].ID,
				CommandFamily: test.family, CheckRoot: test.root, CommandClass: test.class,
			}
			if got := Reduce(obligations, []execution.Event{event}); got[0].Status != test.want {
				t.Fatalf("Reduce() status = %q, want %q", got[0].Status, test.want)
			}
		})
	}
}

func TestReduceQuiescenceRemainsPending(t *testing.T) {
	obligations := []Obligation{{ID: "quiescence", Kind: KindQuiescence, Paths: []string{"."}, Status: StatusPending}}
	events := []execution.Event{{Type: execution.EventVerification, Paths: []string{"."}, Detail: "quiescence", Passed: true}}
	for _, input := range [][]execution.Event{nil, events} {
		decision := Assess(ModeEnforce, true, obligations, input)
		if decision.Allowed || !decision.Disagreement || decision.Obligations[0].Status != StatusPending {
			t.Fatalf("quiescence decision = %#v, want pending and blocked", decision)
		}
	}
}

func TestReduceLaterWritesStaleCoveredInputs(t *testing.T) {
	obligations := []Obligation{{ID: "test", Kind: KindTest, CommandClass: execution.CommandTest, Paths: []string{"pkg/a.go", "pkg/b.go"}, Status: StatusPending}}
	for _, test := range []struct {
		name, write string
		want        Status
	}{
		{name: "one input", write: "pkg/a.go", want: StatusStale},
		{name: "parent directory", write: "pkg", want: StatusStale},
		{name: "workspace root", write: ".", want: StatusStale},
		{name: "unrelated sibling", write: "pkg/c.go", want: StatusSatisfied},
		{name: "unrelated prefix", write: "pkg-other/a.go", want: StatusSatisfied},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []execution.Event{
				{Type: execution.EventVerification, Paths: []string{"pkg/a.go"}, CommandClass: execution.CommandTest, Passed: true},
				{Type: execution.EventVerification, Paths: []string{"pkg/b.go"}, CommandClass: execution.CommandTest, Passed: true},
				{Type: execution.EventWrite, Paths: []string{test.write}},
			}
			if got := Reduce(obligations, events); got[0].Status != test.want {
				t.Fatalf("status after write = %q, want %q", got[0].Status, test.want)
			}
			events = append(events, execution.Event{Type: execution.EventVerification, Paths: []string{"pkg/a.go"}, CommandClass: execution.CommandTest, Passed: true})
			want := StatusSatisfied
			if test.write == "pkg" || test.write == "." {
				want = StatusStale
			}
			if got := Reduce(obligations, events); got[0].Status != want {
				t.Fatalf("status after rechecking one input = %q, want %q", got[0].Status, want)
			}
			events = append(events, execution.Event{Type: execution.EventVerification, Paths: []string{"pkg/b.go"}, CommandClass: execution.CommandTest, Passed: true})
			if got := Reduce(obligations, events); got[0].Status != StatusSatisfied {
				t.Fatalf("status after rechecking all inputs = %q, want satisfied", got[0].Status)
			}
		})
	}
}
