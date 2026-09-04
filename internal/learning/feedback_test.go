package learning

import (
	"testing"
	"time"
)

func TestTracker_RegisterAndRecord(t *testing.T) {
	tr := NewTracker()
	tr.RegisterLearned("search-first", "learn_abc")
	tr.RecordFeedback("search-first", Signal{Kind: SignalPositive, Detail: "good result"})

	pending := tr.PendingFeedback()
	sigs, ok := pending["learn_abc"]
	if !ok || len(sigs) != 1 {
		t.Fatalf("PendingFeedback() = %v, want 1 signal for learn_abc", pending)
	}
	if sigs[0].Kind != SignalPositive || sigs[0].Detail != "good result" {
		t.Errorf("signal = %+v, want positive/good result", sigs[0])
	}
}

func TestTracker_FeedbackForNonLearned(t *testing.T) {
	tr := NewTracker()
	tr.RecordFeedback("coder", Signal{Kind: SignalPositive})

	pending := tr.PendingFeedback()
	if len(pending) != 0 {
		t.Errorf("PendingFeedback() = %v, want empty for non-learned agent", pending)
	}
}

func TestTracker_MultipleSignals(t *testing.T) {
	tr := NewTracker()
	tr.RegisterLearned("agent-a", "learn_1")
	tr.RegisterLearned("agent-b", "learn_2")

	tr.RecordFeedback("agent-a", Signal{Kind: SignalPositive})
	tr.RecordFeedback("agent-a", Signal{Kind: SignalPositive})
	tr.RecordFeedback("agent-b", Signal{Kind: SignalNegative})

	pending := tr.PendingFeedback()
	if len(pending["learn_1"]) != 2 {
		t.Errorf("learn_1 signals = %d, want 2", len(pending["learn_1"]))
	}
	if len(pending["learn_2"]) != 1 {
		t.Errorf("learn_2 signals = %d, want 1", len(pending["learn_2"]))
	}
}

func TestTracker_PendingFeedbackIsSnapshot(t *testing.T) {
	tr := NewTracker()
	tr.RegisterLearned("a", "sug_1")
	tr.RecordFeedback("a", Signal{Kind: SignalPositive})

	snap := tr.PendingFeedback()
	tr.RecordFeedback("a", Signal{Kind: SignalNegative})

	if len(snap["sug_1"]) != 1 {
		t.Error("snapshot was mutated by subsequent RecordFeedback")
	}
}

func TestTracker_ApplyFeedback(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sug := &Suggestion{ID: "learn_apply", Kind: "pattern", Title: "test", Confidence: 0.5}
	if err := store.Save(sug); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	tr := NewTracker()
	tr.RegisterLearned("agent-x", "learn_apply")
	tr.RecordFeedback("agent-x", Signal{Kind: SignalPositive})
	tr.RecordFeedback("agent-x", Signal{Kind: SignalPositive})

	n, err := tr.ApplyFeedback(store)
	if err != nil {
		t.Fatalf("ApplyFeedback() error = %v", err)
	}
	if n != 1 {
		t.Errorf("ApplyFeedback() updated = %d, want 1", n)
	}

	got, err := store.Get("learn_apply")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	// 0.5 + 2*0.05 = 0.6
	if got.Confidence < 0.59 || got.Confidence > 0.61 {
		t.Errorf("Confidence = %f, want ~0.6", got.Confidence)
	}

	// Pending should be cleared after apply.
	if pending := tr.PendingFeedback(); len(pending) != 0 {
		t.Errorf("PendingFeedback after apply = %v, want empty", pending)
	}
}

func TestTracker_ApplyFeedback_MixedSignals(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sug := &Suggestion{ID: "learn_mixed", Kind: "agent", AgentID: "x", Title: "test", Confidence: 0.5, YAML: "id: x\n"}
	if err := store.Save(sug); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	tr := NewTracker()
	tr.RegisterLearned("x", "learn_mixed")
	tr.RecordFeedback("x", Signal{Kind: SignalPositive})   // +0.05
	tr.RecordFeedback("x", Signal{Kind: SignalNegative})   // -0.10
	tr.RecordFeedback("x", Signal{Kind: SignalCorrection}) // -0.15
	// net delta = -0.20, so 0.5 - 0.20 = 0.30

	n, err := tr.ApplyFeedback(store)
	if err != nil {
		t.Fatalf("ApplyFeedback() error = %v", err)
	}
	if n != 1 {
		t.Errorf("updated = %d, want 1", n)
	}

	got, _ := store.Get("learn_mixed")
	if got.Confidence < 0.29 || got.Confidence > 0.31 {
		t.Errorf("Confidence = %f, want ~0.30", got.Confidence)
	}
}

func TestTracker_ApplyFeedback_ClampsAtZero(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sug := &Suggestion{ID: "learn_clamp0", Kind: "pattern", Title: "t", Confidence: 0.05}
	if err := store.Save(sug); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	tr := NewTracker()
	tr.RegisterLearned("a", "learn_clamp0")
	tr.RecordFeedback("a", Signal{Kind: SignalCorrection}) // -0.15, clamped to 0
	tr.ApplyFeedback(store)

	got, _ := store.Get("learn_clamp0")
	if got.Confidence != 0.0 {
		t.Errorf("Confidence = %f, want 0.0 (clamped)", got.Confidence)
	}
}

func TestTracker_TimestampAutoSet(t *testing.T) {
	tr := NewTracker()
	tr.RegisterLearned("a", "s1")
	before := time.Now()
	tr.RecordFeedback("a", Signal{Kind: SignalPositive})
	after := time.Now()

	sigs := tr.PendingFeedback()["s1"]
	if len(sigs) != 1 {
		t.Fatal("expected 1 signal")
	}
	if sigs[0].Timestamp.Before(before) || sigs[0].Timestamp.After(after) {
		t.Errorf("timestamp %v not in [%v, %v]", sigs[0].Timestamp, before, after)
	}
}

func TestUpdateConfidence_Basic(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sug := &Suggestion{ID: "uc_basic", Kind: "pattern", Title: "t", Confidence: 0.5}
	store.Save(sug)

	got, err := store.UpdateConfidence("uc_basic", 0.2)
	if err != nil {
		t.Fatalf("UpdateConfidence() error = %v", err)
	}
	if got.Confidence < 0.69 || got.Confidence > 0.71 {
		t.Errorf("Confidence = %f, want ~0.7", got.Confidence)
	}
}

func TestUpdateConfidence_ClampsHigh(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	store.Save(&Suggestion{ID: "uc_high", Kind: "pattern", Title: "t", Confidence: 0.95})

	got, _ := store.UpdateConfidence("uc_high", 0.2)
	if got.Confidence != 1.0 {
		t.Errorf("Confidence = %f, want 1.0 (clamped)", got.Confidence)
	}
}

func TestUpdateConfidence_ClampsLow(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	store.Save(&Suggestion{ID: "uc_low", Kind: "pattern", Title: "t", Confidence: 0.05})

	got, _ := store.UpdateConfidence("uc_low", -0.3)
	if got.Confidence != 0.0 {
		t.Errorf("Confidence = %f, want 0.0 (clamped)", got.Confidence)
	}
}

func TestUpdateConfidence_NotFound(t *testing.T) {
	store := NewStore(t.TempDir())
	_, err := store.UpdateConfidence("nonexistent", 0.1)
	if err == nil {
		t.Error("UpdateConfidence() with unknown id: want error, got nil")
	}
}
