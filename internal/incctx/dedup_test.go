package incctx

import (
	"strings"
	"testing"
)

func TestQuestionDedup_ExactMatch(t *testing.T) {
	d := NewQuestionDedup()
	d.Record("what does BuildAgent do", "it constructs an agent from config", 3)

	got, ok := d.Check("what does BuildAgent do")
	if !ok {
		t.Fatal("expected match for exact question")
	}
	if !strings.Contains(got, "turn 3") {
		t.Errorf("expected turn reference, got %q", got)
	}
	if !strings.Contains(got, "constructs an agent") {
		t.Errorf("expected answer preview, got %q", got)
	}
}

func TestQuestionDedup_RewordedMatch(t *testing.T) {
	d := NewQuestionDedup()
	d.Record("what does BuildAgent function do in config", "it constructs an agent from config", 5)

	got, ok := d.Check("what does the BuildAgent function do in config")
	if !ok {
		t.Fatal("expected match for reworded question with high overlap")
	}
	if !strings.Contains(got, "turn 5") {
		t.Errorf("expected turn 5 reference, got %q", got)
	}
}

func TestQuestionDedup_NoMatchUnrelated(t *testing.T) {
	d := NewQuestionDedup()
	d.Record("what does BuildAgent do", "it constructs an agent from config", 3)

	_, ok := d.Check("how does the HTTP server handle CORS")
	if ok {
		t.Fatal("expected no match for unrelated question")
	}
}

func TestQuestionDedup_ThresholdPreventsLooseMatch(t *testing.T) {
	d := NewQuestionDedup()
	d.Record("what does BuildAgent do in the SDK package", "it builds agents", 2)

	_, ok := d.Check("what does the SDK package export")
	if ok {
		t.Fatal("expected no match — overlap too low despite shared words")
	}
}

func TestQuestionDedup_MultipleQuestions(t *testing.T) {
	d := NewQuestionDedup()
	d.Record("where is the config loaded", "in internal/config/config.go", 1)
	d.Record("what tests cover BuildAgent", "TestBuildAgent in config_test.go", 4)

	got1, ok1 := d.Check("where is the config loaded")
	if !ok1 {
		t.Fatal("expected match for first question")
	}
	if !strings.Contains(got1, "turn 1") {
		t.Errorf("wrong turn for first question: %q", got1)
	}

	got2, ok2 := d.Check("what tests cover BuildAgent")
	if !ok2 {
		t.Fatal("expected match for second question")
	}
	if !strings.Contains(got2, "turn 4") {
		t.Errorf("wrong turn for second question: %q", got2)
	}
}

func TestQuestionDedup_EmptyQuestion(t *testing.T) {
	d := NewQuestionDedup()
	d.Record("what does BuildAgent do", "it builds agents", 1)

	_, ok := d.Check("")
	if ok {
		t.Fatal("expected no match for empty question")
	}
}

func TestQuestionDedup_AnswerPreviewTruncated(t *testing.T) {
	d := NewQuestionDedup()
	longAnswer := strings.Repeat("x", 300)
	d.Record("what is this", longAnswer, 1)

	got, ok := d.Check("what is this")
	if !ok {
		t.Fatal("expected match")
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected truncated answer, got %q", got)
	}
	if len(got) > 200 {
		t.Errorf("answer preview too long: %d chars", len(got))
	}
}

func TestNormalize_OrderInvariant(t *testing.T) {
	a := normalize("what does BuildAgent do")
	b := normalize("does BuildAgent what do")
	if a != b {
		t.Errorf("normalize not order-invariant: %q vs %q", a, b)
	}
}

func TestNormalize_StripsShortTokens(t *testing.T) {
	got := normalize("I am a test")
	if strings.Contains(got, " a ") || strings.HasPrefix(got, "a ") {
		t.Errorf("expected single-char tokens stripped, got %q", got)
	}
}

func TestJaccardSimilarity(t *testing.T) {
	a := toSet([]string{"the", "quick", "brown", "fox"})
	b := toSet([]string{"the", "quick", "brown", "dog"})
	sim := jaccardSimilarity(a, b)
	expected := 3.0 / 5.0 // 3 shared out of 5 unique
	if sim < expected-0.01 || sim > expected+0.01 {
		t.Errorf("jaccard(%v, %v) = %f, want ~%f", a, b, sim, expected)
	}
}

func TestJaccardSimilarity_Identical(t *testing.T) {
	a := toSet([]string{"hello", "world"})
	sim := jaccardSimilarity(a, a)
	if sim != 1.0 {
		t.Errorf("identical sets: jaccard = %f, want 1.0", sim)
	}
}

func TestJaccardSimilarity_Disjoint(t *testing.T) {
	a := toSet([]string{"hello", "world"})
	b := toSet([]string{"foo", "bar"})
	sim := jaccardSimilarity(a, b)
	if sim != 0.0 {
		t.Errorf("disjoint sets: jaccard = %f, want 0.0", sim)
	}
}

func TestJaccardSimilarity_Empty(t *testing.T) {
	a := toSet([]string{})
	b := toSet([]string{})
	sim := jaccardSimilarity(a, b)
	if sim != 0.0 {
		t.Errorf("empty sets: jaccard = %f, want 0.0", sim)
	}
}
