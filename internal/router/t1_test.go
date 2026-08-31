package router

import (
	"context"
	"errors"
	"testing"
)

// fakeClassifier is a test double for Classifier that returns a canned
// intent/error without calling any real model provider.
type fakeClassifier struct {
	intent string
	err    error
	calls  int
}

func (f *fakeClassifier) Classify(ctx context.Context, message string) (string, error) {
	f.calls++
	return f.intent, f.err
}

func TestClassifyWithFallback_T0MatchSkipsT1(t *testing.T) {
	cfg, err := Parse([]byte(routingYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	r, err := New(cfg, "coder")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	fc := &fakeClassifier{intent: "debug"}
	r.SetT1(fc)

	intent, agent, matched := r.ClassifyWithFallback(context.Background(), "please explain how this works")
	if !matched || intent != "explain" || agent != "explainer" {
		t.Fatalf("ClassifyWithFallback() = (%q, %q, %v), want (%q, %q, true)", intent, agent, matched, "explain", "explainer")
	}
	if fc.calls != 0 {
		t.Errorf("T1 classifier called %d times, want 0 when T0 already matched", fc.calls)
	}
}

func TestClassifyWithFallback_UsesT1WhenT0Unmatched(t *testing.T) {
	cfg, err := Parse([]byte(routingYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	r, err := New(cfg, "coder")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	fc := &fakeClassifier{intent: "review"}
	r.SetT1(fc)

	intent, agent, matched := r.ClassifyWithFallback(context.Background(), "xyzzy plugh")
	if !matched || intent != "review" || agent != "reviewer" {
		t.Fatalf("ClassifyWithFallback() = (%q, %q, %v), want (%q, %q, true)", intent, agent, matched, "review", "reviewer")
	}
	if fc.calls != 1 {
		t.Errorf("T1 classifier called %d times, want 1", fc.calls)
	}
}

func TestClassifyWithFallback_T1ErrorFallsBackUnmatched(t *testing.T) {
	cfg, err := Parse([]byte(routingYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	r, err := New(cfg, "coder")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	r.SetT1(&fakeClassifier{err: errors.New("provider unavailable")})

	intent, agent, matched := r.ClassifyWithFallback(context.Background(), "xyzzy plugh")
	if matched || intent != "code" || agent != "coder" {
		t.Fatalf("ClassifyWithFallback() = (%q, %q, %v), want (%q, %q, false)", intent, agent, matched, "code", "coder")
	}
}

func TestClassifyWithFallback_T1UnknownIntentFallsBackUnmatched(t *testing.T) {
	cfg, err := Parse([]byte(routingYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	r, err := New(cfg, "coder")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	r.SetT1(&fakeClassifier{intent: "not-a-real-intent"})

	intent, agent, matched := r.ClassifyWithFallback(context.Background(), "xyzzy plugh")
	if matched || intent != "code" || agent != "coder" {
		t.Fatalf("ClassifyWithFallback() = (%q, %q, %v), want (%q, %q, false)", intent, agent, matched, "code", "coder")
	}
}

func TestClassifyWithFallback_NoT1ConfiguredFallsBackUnmatched(t *testing.T) {
	cfg, err := Parse([]byte(routingYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	r, err := New(cfg, "coder")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	intent, agent, matched := r.ClassifyWithFallback(context.Background(), "xyzzy plugh")
	if matched || intent != "code" || agent != "coder" {
		t.Fatalf("ClassifyWithFallback() = (%q, %q, %v), want (%q, %q, false)", intent, agent, matched, "code", "coder")
	}
}

func TestNewT1Classifier_NilWithoutPromptOrProvider(t *testing.T) {
	cfg := &Config{}
	if c := NewT1Classifier(nil, cfg); c != nil {
		t.Errorf("NewT1Classifier(nil provider) = %v, want nil", c)
	}
}
