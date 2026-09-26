package graph

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos/storage"
)

func contextCall(t *testing.T, ctx context.Context, s *IndexScope, args map[string]any) *evidenceResult {
	t.Helper()
	out, err := toolFrom(t, s.Tools(), "codebase_context").Handler(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	return out.(*evidenceResult)
}

// The serialized result never exceeds max_tokens by the exact tokenizer,
// for any budget and query, on this repository.
func TestIndexedEvidenceBudgetNeverExceeded(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	s := newTestScope(t, root, false)
	counter, _ := evidenceCounter()
	ctx := context.Background()
	for _, q := range []string{"Reconcile", "how does the watcher debounce edits", "Engine.Update Snapshot Release", "indexer/engine.go"} {
		for _, budget := range []int{256, 512, 1024, 4096, 16384} {
			r := contextCall(t, ctx, s, map[string]any{"query": q, "max_tokens": budget})
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if n := counter.CountString(string(data)); n > budget {
				t.Fatalf("%q at %d tokens: result is %d tokens", q, budget, n)
			}
			if budget >= 4096 && len(r.Items) == 0 {
				t.Fatalf("%q at %d tokens: no items (%v)", q, budget, r.Omitted)
			}
		}
	}
}

func TestIndexedEvidenceSeenAndInvalidation(t *testing.T) {
	root := canonicalTempDir(t)
	writeTree(t, root, payFiles)
	s := newTestScope(t, root, true)
	ctx := storage.WithSession(context.Background(), "sess-a")
	args := map[string]any{"query": "record", "max_tokens": 4096}
	first := contextCall(t, ctx, s, args)
	excerpts := 0
	for _, it := range first.Items {
		if it.Zoom == "excerpt" && it.Source != nil && it.Source.Text != "" {
			excerpts++
		}
		if it.Why == "" {
			t.Fatalf("item without a reason: %+v", it)
		}
	}
	if excerpts == 0 || first.Index == nil {
		t.Fatalf("first call: %+v", first)
	}
	second := contextCall(t, ctx, s, args)
	if second.Omitted["already_seen"] == 0 {
		t.Fatalf("second call repeated delivered source: %v", second.Omitted)
	}
	other := contextCall(t, storage.WithSession(context.Background(), "sess-b"), s, args)
	if other.Omitted["already_seen"] != 0 {
		t.Fatalf("seen state leaked across sessions: %v", other.Omitted)
	}
	writeTree(t, root, map[string]string{"service.go": payFiles["service.go"] + "\n// touched\n"})
	if _, err := s.Engine().Update(context.Background(), []string{"service.go"}); err != nil {
		t.Fatal(err)
	}
	third := contextCall(t, ctx, s, args)
	if len(third.Invalidated) == 0 || third.Invalidated[0] != "service.go" {
		t.Fatalf("changed file not reported: %+v", third.Invalidated)
	}
	miss := contextCall(t, ctx, s, map[string]any{"query": "NoSuchThing", "max_tokens": 1024})
	if len(miss.Misses) != 1 || miss.Omitted["not_found"] == 0 {
		t.Fatalf("miss: %+v", miss)
	}
}

func TestPrefetch(t *testing.T) {
	root := canonicalTempDir(t)
	writeTree(t, root, payFiles)
	s := newTestScope(t, root, false)
	ctx := storage.WithSession(context.Background(), "sess-p")
	text, err := s.Prefetch(ctx, "The record step in Card.Pay loses the amount; fix it", 1500)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "[Repository context]") || !strings.Contains(text, "Card.Pay") || !strings.Contains(text, "func record") {
		t.Fatalf("prefetch:\n%s", text)
	}
	counter, _ := evidenceCounter()
	for _, budget := range []int{64, 200, 1500} {
		text, err := s.Prefetch(storage.WithSession(context.Background(), "b"), "Card.Pay record Handle", budget)
		if err != nil {
			t.Fatal(err)
		}
		if n := counter.CountString(text); n > budget {
			t.Fatalf("prefetch at %d tokens is %d tokens", budget, n)
		}
	}
	if text, _ := s.Prefetch(ctx, "hello, how are you today?", 1500); text != "" {
		t.Fatalf("chat text produced context:\n%s", text)
	}
	// Source delivered by prefetch is not repeated by codebase_context.
	r := contextCall(t, ctx, s, map[string]any{"query": "Card.Pay", "max_tokens": 4096})
	if r.Omitted["already_seen"] == 0 {
		t.Fatalf("prefetched source repeated: %v", r.Omitted)
	}
}
