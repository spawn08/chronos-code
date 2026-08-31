package learning

import (
	"sync"
	"time"
)

// SignalKind classifies a user feedback signal.
type SignalKind string

const (
	SignalPositive   SignalKind = "positive"
	SignalNegative   SignalKind = "negative"
	SignalCorrection SignalKind = "correction"
)

// Signal is a single feedback event from the user about an agent's behavior.
type Signal struct {
	Kind      SignalKind `yaml:"kind"`
	Detail    string     `yaml:"detail,omitempty"`
	Timestamp time.Time  `yaml:"timestamp"`
}

// signalDelta maps each signal kind to the confidence adjustment it produces
// when applied. Positive signals are a gentle nudge up; negative and
// correction signals push harder in the opposite direction, reflecting that
// it is easier to erode trust than to build it.
var signalDelta = map[SignalKind]float64{
	SignalPositive:   +0.05,
	SignalNegative:   -0.10,
	SignalCorrection: -0.15,
}

// Tracker records which accepted learned agents are currently active and
// collects user feedback signals to close the learning loop (PRD P4-003).
type Tracker struct {
	mu       sync.Mutex
	active   map[string]string   // agentID -> suggestion ID
	feedback map[string][]Signal // suggestion ID -> signals
}

// NewTracker creates an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{
		active:   make(map[string]string),
		feedback: make(map[string][]Signal),
	}
}

// RegisterLearned marks agentID as originating from the learning loop,
// associated with suggestionID. Subsequent feedback for this agent is
// attributed to the suggestion.
func (t *Tracker) RegisterLearned(agentID, suggestionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active[agentID] = suggestionID
}

// RecordFeedback records a feedback signal for agentID. If the agent
// originated from the learning loop, the signal is grouped under its
// suggestion ID for later confidence adjustment.
func (t *Tracker) RecordFeedback(agentID string, signal Signal) {
	if signal.Timestamp.IsZero() {
		signal.Timestamp = time.Now()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	sugID, ok := t.active[agentID]
	if !ok {
		return
	}
	t.feedback[sugID] = append(t.feedback[sugID], signal)
}

// PendingFeedback returns all accumulated feedback signals grouped by
// suggestion ID. The returned map is a snapshot; the tracker's internal
// state is not modified.
func (t *Tracker) PendingFeedback() map[string][]Signal {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string][]Signal, len(t.feedback))
	for id, sigs := range t.feedback {
		cp := make([]Signal, len(sigs))
		copy(cp, sigs)
		out[id] = cp
	}
	return out
}

// ApplyFeedback reads pending feedback and updates suggestion confidence in
// store: positive signals increase by +0.05, negative decrease by -0.10,
// corrections decrease by -0.15. Applied signals are removed from the
// tracker. Returns the number of suggestions updated.
func (t *Tracker) ApplyFeedback(store *Store) (int, error) {
	t.mu.Lock()
	pending := t.feedback
	t.feedback = make(map[string][]Signal)
	t.mu.Unlock()

	updated := 0
	for sugID, signals := range pending {
		var totalDelta float64
		for _, sig := range signals {
			totalDelta += signalDelta[sig.Kind]
		}
		if totalDelta == 0 {
			continue
		}
		if _, err := store.UpdateConfidence(sugID, totalDelta); err != nil {
			t.mu.Lock()
			t.feedback[sugID] = append(t.feedback[sugID], signals...)
			t.mu.Unlock()
			return updated, err
		}
		updated++
	}
	return updated, nil
}
