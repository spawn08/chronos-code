// Package execution defines the append-only evidence ledger for one task.
package execution

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrDuplicateEventID    = errors.New("duplicate evidence event ID")
	ErrDuplicateEvidenceID = errors.New("duplicate verification evidence ID")
	ErrDuplicateCallID     = errors.New("duplicate call ID")
	ErrOutOfOrderEvent     = errors.New("evidence events must be ordered")
	ErrTaskMismatch        = errors.New("evidence event task mismatch")
	ErrInvalidEvent        = errors.New("invalid evidence event")
)

type (
	TaskID     string
	EventID    string
	CallID     string
	EvidenceID string
	EventType  string
)

const (
	EventRequirement  EventType = "requirement"
	EventAssumption   EventType = "assumption"
	EventWrite        EventType = "write"
	EventModelCall    EventType = "model_call"
	EventToolCall     EventType = "tool_call"
	EventVerification EventType = "verification"
	EventUncertainty  EventType = "uncertainty"
)

// Event is one immutable fact about a task. Sequence is assigned by the
// durable store and defines the only valid reduction order.
type Event struct {
	ID         EventID
	TaskID     TaskID
	Sequence   uint64
	Type       EventType
	CallID     CallID
	EvidenceID EvidenceID
	Paths      []string
	Passed     bool
	Detail     string
}

// Verification is a verification event together with whether later writes
// have made its result stale.
type Verification struct {
	Event   Event
	Current bool
}

// State is the deterministic current view of ordered ledger events.
type State struct {
	TaskID        TaskID
	Events        []Event
	Requirements  []Event
	Assumptions   []Event
	Writes        []Event
	Calls         map[CallID]Event
	Verification  map[EvidenceID]Verification
	Uncertainties []Event
}

// Ledger retains append-only events in memory. Persistence supplies the same
// ordered events to Reduce after a process restart.
type Ledger struct {
	mu     sync.RWMutex
	taskID TaskID
	events []Event
}

// NewLedger creates a ledger that accepts events for taskID only.
func NewLedger(taskID TaskID) *Ledger {
	return &Ledger{taskID: taskID}
}

// Append validates event ordering against the existing append-only history.
func (l *Ledger) Append(event Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	events := make([]Event, len(l.events), len(l.events)+1)
	copy(events, l.events)
	events = append(events, cloneEvent(event))
	state, err := Reduce(events)
	if err != nil {
		return err
	}
	if l.taskID != "" && state.TaskID != l.taskID {
		return fmt.Errorf("%w: got %q, want %q", ErrTaskMismatch, event.TaskID, l.taskID)
	}
	if l.taskID == "" {
		l.taskID = state.TaskID
	}
	l.events = events
	return nil
}

// Events returns a copy so callers cannot alter ledger history.
func (l *Ledger) Events() []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return cloneEvents(l.events)
}

// State reduces a consistent snapshot of the ledger.
func (l *Ledger) State() (State, error) {
	l.mu.RLock()
	events := cloneEvents(l.events)
	l.mu.RUnlock()
	return Reduce(events)
}

// Reduce derives current task state from ordered persisted events.
func Reduce(events []Event) (State, error) {
	state := State{
		Calls:        make(map[CallID]Event),
		Verification: make(map[EvidenceID]Verification),
	}
	seenEvents := make(map[EventID]struct{}, len(events))
	var previous uint64

	for i, input := range events {
		event := cloneEvent(input)
		if err := validateEvent(event); err != nil {
			return State{}, fmt.Errorf("event %d: %w", i, err)
		}
		if _, exists := seenEvents[event.ID]; exists {
			return State{}, fmt.Errorf("event %d %q: %w", i, event.ID, ErrDuplicateEventID)
		}
		if i > 0 && event.Sequence <= previous {
			return State{}, fmt.Errorf("event %d sequence %d follows %d: %w", i, event.Sequence, previous, ErrOutOfOrderEvent)
		}
		if state.TaskID == "" {
			state.TaskID = event.TaskID
		} else if event.TaskID != state.TaskID {
			return State{}, fmt.Errorf("event %d: %w: got %q, want %q", i, ErrTaskMismatch, event.TaskID, state.TaskID)
		}

		seenEvents[event.ID] = struct{}{}
		previous = event.Sequence
		state.Events = append(state.Events, event)
		switch event.Type {
		case EventRequirement:
			state.Requirements = append(state.Requirements, event)
		case EventAssumption:
			state.Assumptions = append(state.Assumptions, event)
		case EventWrite:
			state.Writes = append(state.Writes, event)
			invalidateVerification(state.Verification, event.Paths)
		case EventModelCall, EventToolCall:
			if _, exists := state.Calls[event.CallID]; exists {
				return State{}, fmt.Errorf("event %d call %q: %w", i, event.CallID, ErrDuplicateCallID)
			}
			state.Calls[event.CallID] = event
		case EventVerification:
			if _, exists := state.Verification[event.EvidenceID]; exists {
				return State{}, fmt.Errorf("event %d evidence %q: %w", i, event.EvidenceID, ErrDuplicateEvidenceID)
			}
			state.Verification[event.EvidenceID] = Verification{Event: event, Current: event.Passed}
		case EventUncertainty:
			state.Uncertainties = append(state.Uncertainties, event)
		}
	}
	return state, nil
}

func validateEvent(event Event) error {
	if event.ID == "" || event.TaskID == "" || event.Sequence == 0 {
		return ErrInvalidEvent
	}
	switch event.Type {
	case EventRequirement, EventAssumption, EventUncertainty:
		return nil
	case EventWrite:
		if len(event.Paths) == 0 {
			return ErrInvalidEvent
		}
	case EventModelCall, EventToolCall:
		if event.CallID == "" {
			return ErrInvalidEvent
		}
	case EventVerification:
		if event.EvidenceID == "" || len(event.Paths) == 0 {
			return ErrInvalidEvent
		}
	default:
		return ErrInvalidEvent
	}
	return nil
}

func invalidateVerification(verification map[EvidenceID]Verification, paths []string) {
	for id, evidence := range verification {
		if !evidence.Current || !pathsOverlap(evidence.Event.Paths, paths) {
			continue
		}
		evidence.Current = false
		verification[id] = evidence
	}
}

func pathsOverlap(left, right []string) bool {
	paths := make(map[string]struct{}, len(left))
	for _, path := range left {
		paths[path] = struct{}{}
	}
	for _, path := range right {
		if _, exists := paths[path]; exists {
			return true
		}
	}
	return false
}

func cloneEvents(events []Event) []Event {
	cloned := make([]Event, len(events))
	for i, event := range events {
		cloned[i] = cloneEvent(event)
	}
	return cloned
}

func cloneEvent(event Event) Event {
	event.Paths = append([]string(nil), event.Paths...)
	return event
}
