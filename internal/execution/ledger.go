// Package execution defines the append-only evidence ledger for one task.
package execution

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrDuplicateEventID     = errors.New("duplicate evidence event ID")
	ErrDuplicateEvidenceID  = errors.New("duplicate verification evidence ID")
	ErrDuplicateCallID      = errors.New("duplicate call ID")
	ErrOutOfOrderEvent      = errors.New("evidence events must be ordered")
	ErrTaskMismatch         = errors.New("evidence event task mismatch")
	ErrInvalidEvent         = errors.New("invalid evidence event")
	ErrPathOutsideWorkspace = errors.New("evidence path is outside workspace")
)

type (
	TaskID             string
	EventID            string
	CallID             string
	EvidenceID         string
	EventType          string
	CommandClass       string
	TerminalState      string
	EvidenceProvenance string
	ScopeKind          string
)

const (
	ScopeExact     ScopeKind = "exact"
	ScopeDirectory ScopeKind = "directory"
	ScopeWorkspace ScopeKind = "workspace"
)

type Scope struct {
	Kind ScopeKind
	Path string
}

const (
	EventRequirement  EventType = "requirement"
	EventAssumption   EventType = "assumption"
	EventWrite        EventType = "write"
	EventModelCall    EventType = "model_call"
	EventToolCall     EventType = "tool_call"
	EventVerification EventType = "verification"
	EventUncertainty  EventType = "uncertainty"
	// EventClaim records a working-memory claim status transition. It is
	// audit-only and never affects verification currency.
	EventClaim EventType = "claim"
)

const (
	CommandUnknown     CommandClass = "unknown"
	CommandBuild       CommandClass = "build"
	CommandDiagnostics CommandClass = "diagnostics"
	CommandTest        CommandClass = "test"
	CommandDiff        CommandClass = "diff"
	CommandRead        CommandClass = "read"
	CommandMutation    CommandClass = "mutation"
)

const (
	TerminalExited      TerminalState = "exited"
	TerminalTimedOut    TerminalState = "timed_out"
	TerminalCancelled   TerminalState = "cancelled"
	TerminalSpawnFailed TerminalState = "spawn_failed"
)

const (
	ProvenanceRuntime        EvidenceProvenance = "runtime"
	ProvenanceTrustedAdapter EvidenceProvenance = "trusted_adapter"
)

// Event is one immutable fact about a task. Sequence defines the only valid
// reduction order and is assigned by Ledger.Record for runtime events.
type Event struct {
	ID         EventID
	TaskID     TaskID
	Sequence   uint64
	Type       EventType
	CallID     CallID
	EvidenceID EvidenceID
	Paths      []string
	Scopes     []Scope
	Passed     bool
	Detail     string

	ContentHash      string
	SizeBytes        int64
	MutationRevision uint64
	Command          string
	CommandClass     CommandClass
	ExitCode         *int
	TerminalState    TerminalState
	Provenance       EvidenceProvenance
	StartedAt        time.Time
	CompletedAt      time.Time
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
	mu       sync.RWMutex
	taskID   TaskID
	events   []Event
	nextID   uint64
	revision uint64
	sink     EventSink
}

// NewLedger creates a ledger that accepts events for taskID only.
func NewLedger(taskID TaskID) *Ledger {
	return &Ledger{taskID: taskID}
}

// Append validates event ordering against the existing append-only history.
func (l *Ledger) Append(event Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(event)
}

// Record assigns task-local identity, ordering, and mutation revision before
// appending event. It is the runtime path; Append remains available for replay
// of events whose durable identity has already been assigned.
func (l *Ledger) Record(event Event) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sequence := uint64(len(l.events) + 1)
	if len(l.events) > 0 {
		sequence = l.events[len(l.events)-1].Sequence + 1
	}
	nextID := l.nextID + 1
	event.ID = EventID(fmt.Sprintf("%s:%d", l.taskID, nextID))
	event.TaskID = l.taskID
	event.Sequence = sequence
	if event.Type == EventVerification && event.EvidenceID == "" {
		event.EvidenceID = EvidenceID(event.ID)
	}
	if (event.Type == EventModelCall || event.Type == EventToolCall) && event.CallID == "" {
		event.CallID = CallID(event.ID)
	}
	revision := l.revision
	if event.Type == EventWrite {
		revision++
		event.MutationRevision = revision
	} else if event.MutationRevision == 0 {
		event.MutationRevision = revision
	}
	if err := l.appendLocked(event); err != nil {
		return Event{}, err
	}
	l.nextID = nextID
	l.revision = revision
	return cloneEvent(event), nil
}

func (l *Ledger) appendLocked(event Event) error {
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
	if l.sink != nil {
		// Persist before committing so durable history is never behind memory.
		if err := l.sink.AppendEvent(cloneEvent(event)); err != nil {
			return fmt.Errorf("persist evidence event %q: %w", event.ID, err)
		}
	}
	if l.taskID == "" {
		l.taskID = state.TaskID
	}
	l.events = events
	if event.Sequence > l.nextID {
		l.nextID = event.Sequence
	}
	if event.MutationRevision > l.revision {
		l.revision = event.MutationRevision
	}
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

// Revision returns the latest task-local mutation revision.
func (l *Ledger) Revision() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.revision
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
			invalidateVerification(state.Verification, event)
		case EventModelCall, EventToolCall:
			if _, exists := state.Calls[event.CallID]; exists {
				return State{}, fmt.Errorf("event %d call %q: %w", i, event.CallID, ErrDuplicateCallID)
			}
			state.Calls[event.CallID] = event
		case EventVerification:
			if _, exists := state.Verification[event.EvidenceID]; exists {
				return State{}, fmt.Errorf("event %d evidence %q: %w", i, event.EvidenceID, ErrDuplicateEvidenceID)
			}
			state.Verification[event.EvidenceID] = Verification{Event: event, Current: event.Passed && verificationRevisionCurrent(state.Writes, event)}
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
	if event.SizeBytes < 0 || (!event.StartedAt.IsZero() && !event.CompletedAt.IsZero() && event.CompletedAt.Before(event.StartedAt)) {
		return ErrInvalidEvent
	}
	if !validCommandClass(event.CommandClass) || !validTerminalState(event.TerminalState) || !validProvenance(event.Provenance) {
		return ErrInvalidEvent
	}
	for _, scope := range event.Scopes {
		if scope.Kind == ScopeWorkspace {
			if scope.Path != "" {
				return ErrInvalidEvent
			}
			continue
		}
		if (scope.Kind != ScopeExact && scope.Kind != ScopeDirectory) || scope.Path == "" {
			return ErrInvalidEvent
		}
	}
	switch event.Type {
	case EventRequirement, EventAssumption, EventUncertainty, EventClaim:
		return nil
	case EventWrite:
		if len(event.Paths) == 0 && len(event.Scopes) == 0 {
			return ErrInvalidEvent
		}
	case EventModelCall, EventToolCall:
		if event.CallID == "" {
			return ErrInvalidEvent
		}
	case EventVerification:
		if event.EvidenceID == "" || (len(event.Paths) == 0 && len(event.Scopes) == 0) {
			return ErrInvalidEvent
		}
	default:
		return ErrInvalidEvent
	}
	return nil
}

// NormalizeScope canonicalizes a scope to a slash-separated path relative to
// workspaceRoot and rejects lexical and symlink escapes. Workspace scope has
// no path and is valid as-is.
func NormalizeScope(workspaceRoot string, scope Scope) (Scope, error) {
	if scope.Kind == ScopeWorkspace {
		if scope.Path != "" {
			return Scope{}, ErrInvalidEvent
		}
		return scope, nil
	}
	if scope.Kind != ScopeExact && scope.Kind != ScopeDirectory || scope.Path == "" {
		return Scope{}, ErrInvalidEvent
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return Scope{}, fmt.Errorf("resolve workspace root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Scope{}, fmt.Errorf("resolve workspace root symlinks: %w", err)
	}
	candidate := scope.Path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = resolveExistingAncestors(filepath.Clean(candidate))
	if err != nil {
		return Scope{}, fmt.Errorf("resolve evidence path: %w", err)
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return Scope{}, fmt.Errorf("relativize evidence path: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return Scope{}, ErrPathOutsideWorkspace
	}
	scope.Path = filepath.ToSlash(relative)
	return scope, nil
}

func resolveExistingAncestors(path string) (string, error) {
	current := path
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func validCommandClass(class CommandClass) bool {
	switch class {
	case "", CommandUnknown, CommandBuild, CommandDiagnostics, CommandTest, CommandDiff, CommandRead, CommandMutation:
		return true
	default:
		return false
	}
}

func validTerminalState(state TerminalState) bool {
	switch state {
	case "", TerminalExited, TerminalTimedOut, TerminalCancelled, TerminalSpawnFailed:
		return true
	default:
		return false
	}
}

func validProvenance(provenance EvidenceProvenance) bool {
	switch provenance {
	case "", ProvenanceRuntime, ProvenanceTrustedAdapter:
		return true
	default:
		return false
	}
}

func invalidateVerification(verification map[EvidenceID]Verification, write Event) {
	for id, evidence := range verification {
		if !evidence.Current || !eventScopesOverlap(evidence.Event, write) {
			continue
		}
		evidence.Current = false
		verification[id] = evidence
	}
}

func verificationRevisionCurrent(writes []Event, verification Event) bool {
	if verification.MutationRevision == 0 {
		return true
	}
	for _, write := range writes {
		if write.MutationRevision > verification.MutationRevision && eventScopesOverlap(verification, write) {
			return false
		}
	}
	return true
}

func eventScopesOverlap(left, right Event) bool {
	if len(left.Scopes) > 0 || len(right.Scopes) > 0 {
		return ScopesOverlap(scopesForEvent(left), scopesForEvent(right))
	}
	return pathsOverlap(left.Paths, right.Paths)
}

func scopesForEvent(event Event) []Scope {
	if len(event.Scopes) > 0 {
		return event.Scopes
	}
	scopes := make([]Scope, 0, len(event.Paths))
	for _, path := range event.Paths {
		scopes = append(scopes, Scope{Kind: ScopeExact, Path: path})
	}
	return scopes
}

// ScopesOverlap reports whether either scope covers any part of the other.
func ScopesOverlap(left, right []Scope) bool {
	for _, l := range left {
		for _, r := range right {
			if scopeOverlaps(l, r) {
				return true
			}
		}
	}
	return false
}

func scopeOverlaps(left, right Scope) bool {
	if left.Kind == ScopeWorkspace || right.Kind == ScopeWorkspace {
		return true
	}
	if left.Path == right.Path {
		return true
	}
	return left.Kind == ScopeDirectory && pathWithin(right.Path, left.Path) ||
		right.Kind == ScopeDirectory && pathWithin(left.Path, right.Path)
}

func pathWithin(path, directory string) bool {
	return strings.HasPrefix(path, strings.TrimSuffix(directory, "/")+"/")
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
	event.Scopes = append([]Scope(nil), event.Scopes...)
	if event.ExitCode != nil {
		exitCode := *event.ExitCode
		event.ExitCode = &exitCode
	}
	return event
}
