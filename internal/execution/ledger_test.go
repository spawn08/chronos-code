package execution

import (
	"errors"
	"reflect"
	"testing"
)

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
