package execution

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestNormalizeScopeUsesWorkspaceRelativeIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(root, "internal", "ledger.go")
	for _, input := range []string{"internal/ledger.go", "./internal/../internal/ledger.go", absolute} {
		scope, err := NormalizeScope(root, Scope{Kind: ScopeExact, Path: input})
		if err != nil {
			t.Fatalf("NormalizeScope(%q) error = %v", input, err)
		}
		if scope.Path != "internal/ledger.go" || scope.Kind != ScopeExact {
			t.Fatalf("NormalizeScope(%q) = %#v", input, scope)
		}
	}
	if _, err := NormalizeScope(root, Scope{Kind: ScopeExact, Path: "../outside.go"}); !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("outside path error = %v, want %v", err, ErrPathOutsideWorkspace)
	}
}

func TestNormalizeScopeRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	_, err := NormalizeScope(root, Scope{Kind: ScopeExact, Path: "escape/new.go"})
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("symlink escape error = %v, want %v", err, ErrPathOutsideWorkspace)
	}
}

func TestScopesOverlapHierarchy(t *testing.T) {
	tests := []struct {
		name        string
		left, right []Scope
		want        bool
	}{
		{name: "same exact", left: []Scope{{Kind: ScopeExact, Path: "a/b.go"}}, right: []Scope{{Kind: ScopeExact, Path: "a/b.go"}}, want: true},
		{name: "directory contains exact", left: []Scope{{Kind: ScopeDirectory, Path: "a"}}, right: []Scope{{Kind: ScopeExact, Path: "a/b.go"}}, want: true},
		{name: "workspace contains exact", left: []Scope{{Kind: ScopeWorkspace}}, right: []Scope{{Kind: ScopeExact, Path: "a/b.go"}}, want: true},
		{name: "prefix is not ancestor", left: []Scope{{Kind: ScopeDirectory, Path: "a"}}, right: []Scope{{Kind: ScopeExact, Path: "ab/b.go"}}},
		{name: "unrelated exact", left: []Scope{{Kind: ScopeExact, Path: "a.go"}}, right: []Scope{{Kind: ScopeExact, Path: "b.go"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ScopesOverlap(test.left, test.right); got != test.want {
				t.Fatalf("ScopesOverlap(%v, %v) = %v, want %v", test.left, test.right, got, test.want)
			}
		})
	}
}

func TestReduceUsesScopesAndMutationRevisionForFreshness(t *testing.T) {
	state, err := Reduce([]Event{
		{ID: "verify-a", TaskID: "task", Sequence: 1, Type: EventVerification, EvidenceID: "a", Scopes: []Scope{{Kind: ScopeDirectory, Path: "pkg/a"}}, Passed: true, MutationRevision: 1},
		{ID: "verify-b", TaskID: "task", Sequence: 2, Type: EventVerification, EvidenceID: "b", Scopes: []Scope{{Kind: ScopeDirectory, Path: "pkg/b"}}, Passed: true, MutationRevision: 1},
		{ID: "write-a", TaskID: "task", Sequence: 3, Type: EventWrite, Scopes: []Scope{{Kind: ScopeExact, Path: "pkg/a/file.go"}}, MutationRevision: 2},
		{ID: "late-stale", TaskID: "task", Sequence: 4, Type: EventVerification, EvidenceID: "late", Scopes: []Scope{{Kind: ScopeDirectory, Path: "pkg/a"}}, Passed: true, MutationRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Verification["a"].Current || state.Verification["late"].Current {
		t.Fatalf("overlapping evidence remained current: %#v", state.Verification)
	}
	if !state.Verification["b"].Current {
		t.Fatalf("non-overlapping evidence became stale: %#v", state.Verification["b"])
	}
}

func TestTypedEvidenceRoundTripsWithoutExposingMutableFields(t *testing.T) {
	exitCode := 1
	started := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	event := Event{
		ID: "check", TaskID: "task-1", Sequence: 1, Type: EventVerification,
		EvidenceID: "evidence-1", Paths: []string{"internal/execution/ledger.go"},
		ContentHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SizeBytes:   42, MutationRevision: 3, Command: "go test ./internal/execution",
		CommandClass: CommandTest, ExitCode: &exitCode, TerminalState: TerminalExited,
		Provenance: ProvenanceRuntime, StartedAt: started, CompletedAt: started.Add(time.Second),
	}
	ledger := NewLedger("task-1")
	if err := ledger.Append(event); err != nil {
		t.Fatal(err)
	}

	events := ledger.Events()
	if !reflect.DeepEqual(events[0], event) {
		t.Fatalf("event = %#v, want %#v", events[0], event)
	}
	*events[0].ExitCode = 0
	events[0].Paths[0] = "changed.go"
	got := ledger.Events()[0]
	if *got.ExitCode != 1 || got.Paths[0] != "internal/execution/ledger.go" {
		t.Fatalf("caller mutated typed ledger history: %#v", got)
	}
}

func TestLedgerRecordAssignsConcurrentOrderedIdentity(t *testing.T) {
	ledger := NewLedger("task-1")
	const count = 64
	recorded := make(chan Event, count)
	var group sync.WaitGroup
	for range count {
		group.Add(1)
		go func() {
			defer group.Done()
			event, err := ledger.Record(Event{Type: EventAssumption, Detail: "concurrent"})
			if err != nil {
				t.Errorf("Record() error = %v", err)
				return
			}
			recorded <- event
		}()
	}
	group.Wait()
	close(recorded)

	sequences := make([]int, 0, count)
	ids := make(map[EventID]struct{}, count)
	for event := range recorded {
		sequences = append(sequences, int(event.Sequence))
		ids[event.ID] = struct{}{}
		if event.TaskID != "task-1" {
			t.Errorf("TaskID = %q, want task-1", event.TaskID)
		}
	}
	sort.Ints(sequences)
	if len(ids) != count {
		t.Fatalf("unique IDs = %d, want %d", len(ids), count)
	}
	for i, sequence := range sequences {
		if sequence != i+1 {
			t.Fatalf("sequences = %v, want contiguous ordering", sequences)
		}
	}
	state, err := ledger.State()
	if err != nil || len(state.Events) != count {
		t.Fatalf("State() events = %d, error = %v", len(state.Events), err)
	}
}

func TestLedgerRecordAssignsMutationRevision(t *testing.T) {
	ledger := NewLedger("task-1")
	first, err := ledger.Record(Event{Type: EventWrite, Paths: []string{"first.go"}})
	if err != nil {
		t.Fatal(err)
	}
	check, err := ledger.Record(Event{Type: EventVerification, EvidenceID: "check", Paths: []string{"first.go"}, Passed: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ledger.Record(Event{Type: EventWrite, Paths: []string{"second.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.MutationRevision != 1 || check.MutationRevision != 1 || second.MutationRevision != 2 {
		t.Fatalf("revisions = (%d, %d, %d), want (1, 1, 2)", first.MutationRevision, check.MutationRevision, second.MutationRevision)
	}
}

func TestLedgerRecordFailureDoesNotConsumeIdentityOrRevision(t *testing.T) {
	ledger := NewLedger("task-1")
	if _, err := ledger.Record(Event{Type: EventWrite}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("invalid Record() error = %v, want %v", err, ErrInvalidEvent)
	}
	event, err := ledger.Record(Event{Type: EventWrite, Paths: []string{"main.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if event.ID != "task-1:1" || event.Sequence != 1 || event.MutationRevision != 1 {
		t.Fatalf("event after rejected record = %#v, want first identity and revision", event)
	}
}

func TestReduceInvalidatesVerificationAfterWrite(t *testing.T) {
	state, err := Reduce([]Event{
		{ID: "verify-source", TaskID: "task-1", Sequence: 1, Type: EventVerification, EvidenceID: "evidence-1", Paths: []string{"main.go"}, Passed: true},
		{ID: "write-other", TaskID: "task-1", Sequence: 2, Type: EventWrite, Paths: []string{"README.md"}},
		{ID: "write-source", TaskID: "task-1", Sequence: 3, Type: EventWrite, Paths: []string{"main.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Verification["evidence-1"].Current {
		t.Fatal("verification remained current after a write to its path")
	}
}

func TestReduceRetainsTaskAndCallIdentity(t *testing.T) {
	state, err := Reduce([]Event{
		{ID: "model", TaskID: "task-1", Sequence: 1, Type: EventModelCall, CallID: "call-model"},
		{ID: "tool", TaskID: "task-1", Sequence: 2, Type: EventToolCall, CallID: "call-tool"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.TaskID != "task-1" || state.Calls["call-model"].TaskID != "task-1" || state.Calls["call-tool"].TaskID != "task-1" {
		t.Fatalf("call identities lost task correlation: %#v", state)
	}
}

func TestReduceIsDeterministic(t *testing.T) {
	events := []Event{
		{ID: "requirement", TaskID: "task-1", Sequence: 1, Type: EventRequirement, Detail: "add ledger"},
		{ID: "verify", TaskID: "task-1", Sequence: 2, Type: EventVerification, EvidenceID: "evidence-1", Paths: []string{"ledger.go"}, Passed: true},
		{ID: "write", TaskID: "task-1", Sequence: 3, Type: EventWrite, Paths: []string{"ledger.go"}},
	}
	first, err := Reduce(events)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Reduce(events)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same ordered events reduced differently:\nfirst: %#v\nsecond: %#v", first, second)
	}
}

func TestReduceRejectsDuplicateAndOutOfOrderEvents(t *testing.T) {
	valid := Event{ID: "event-1", TaskID: "task-1", Sequence: 1, Type: EventRequirement}
	tests := []struct {
		name   string
		events []Event
		want   error
	}{
		{
			name:   "duplicate event ID",
			events: []Event{valid, {ID: "event-1", TaskID: "task-1", Sequence: 2, Type: EventAssumption}},
			want:   ErrDuplicateEventID,
		},
		{
			name:   "out of order sequence",
			events: []Event{valid, {ID: "event-2", TaskID: "task-1", Sequence: 1, Type: EventAssumption}},
			want:   ErrOutOfOrderEvent,
		},
		{
			name: "duplicate call ID",
			events: []Event{
				{ID: "event-1", TaskID: "task-1", Sequence: 1, Type: EventModelCall, CallID: "call-1"},
				{ID: "event-2", TaskID: "task-1", Sequence: 2, Type: EventToolCall, CallID: "call-1"},
			},
			want: ErrDuplicateCallID,
		},
		{
			name: "duplicate verification evidence ID",
			events: []Event{
				{ID: "event-1", TaskID: "task-1", Sequence: 1, Type: EventVerification, EvidenceID: "evidence-1", Paths: []string{"main.go"}, Passed: true},
				{ID: "event-2", TaskID: "task-1", Sequence: 2, Type: EventVerification, EvidenceID: "evidence-1", Paths: []string{"main.go"}, Passed: true},
			},
			want: ErrDuplicateEvidenceID,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Reduce(test.events)
			if !errors.Is(err, test.want) {
				t.Fatalf("Reduce() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestLedgerDoesNotExposeMutableHistory(t *testing.T) {
	ledger := NewLedger("task-1")
	if err := ledger.Append(Event{ID: "write", TaskID: "task-1", Sequence: 1, Type: EventWrite, Paths: []string{"main.go"}}); err != nil {
		t.Fatal(err)
	}
	events := ledger.Events()
	events[0].Paths[0] = "changed.go"
	state, err := ledger.State()
	if err != nil {
		t.Fatal(err)
	}
	if state.Writes[0].Paths[0] != "main.go" {
		t.Fatal("caller mutated append-only ledger history")
	}
}
