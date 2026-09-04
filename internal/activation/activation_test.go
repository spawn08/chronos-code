package activation

import (
	"context"
	"testing"

	"github.com/spawn08/chronos-code/internal/graph"
)

func TestBuffer_PutGet(t *testing.T) {
	buf := NewBuffer(3)
	e := &Entry{Symbol: graph.Symbol{Name: "Foo", Kind: graph.KindFunc}}
	buf.Put("Foo", e)

	got, ok := buf.Get("Foo")
	if !ok {
		t.Fatal("expected hit")
	}
	if got.Symbol.Name != "Foo" {
		t.Fatalf("got %q, want Foo", got.Symbol.Name)
	}
}

func TestBuffer_LRUEviction(t *testing.T) {
	buf := NewBuffer(3)
	buf.Put("A", &Entry{Symbol: graph.Symbol{Name: "A"}})
	buf.Put("B", &Entry{Symbol: graph.Symbol{Name: "B"}})
	buf.Put("C", &Entry{Symbol: graph.Symbol{Name: "C"}})
	buf.Put("D", &Entry{Symbol: graph.Symbol{Name: "D"}}) // evicts A

	if _, ok := buf.Get("A"); ok {
		t.Fatal("A should have been evicted")
	}
	if _, ok := buf.Get("D"); !ok {
		t.Fatal("D should be present")
	}
	if buf.Len() != 3 {
		t.Fatalf("len=%d, want 3", buf.Len())
	}
}

func TestBuffer_PromotePreventsEviction(t *testing.T) {
	buf := NewBuffer(3)
	buf.Put("A", &Entry{Symbol: graph.Symbol{Name: "A"}})
	buf.Put("B", &Entry{Symbol: graph.Symbol{Name: "B"}})
	buf.Put("C", &Entry{Symbol: graph.Symbol{Name: "C"}})
	buf.Get("A")                                          // promote A
	buf.Put("D", &Entry{Symbol: graph.Symbol{Name: "D"}}) // evicts B (oldest after promotion)

	if _, ok := buf.Get("A"); !ok {
		t.Fatal("A should still be present after promotion")
	}
	if _, ok := buf.Get("B"); ok {
		t.Fatal("B should have been evicted")
	}
}

func TestBuffer_Stats(t *testing.T) {
	buf := NewBuffer(10)
	buf.Put("X", &Entry{Symbol: graph.Symbol{Name: "X"}})
	buf.Get("X")
	buf.Get("Y")
	buf.Get("X")

	hits, misses := buf.Stats()
	if hits != 2 || misses != 1 {
		t.Fatalf("hits=%d misses=%d, want 2/1", hits, misses)
	}

	rate := buf.HitRate()
	expected := 2.0 / 3.0
	if rate < expected-0.01 || rate > expected+0.01 {
		t.Fatalf("hit rate=%f, want ~%f", rate, expected)
	}
}

func TestBuffer_HitRateEmpty(t *testing.T) {
	buf := NewBuffer(10)
	if rate := buf.HitRate(); rate != 0 {
		t.Fatalf("empty buffer hit rate=%f, want 0", rate)
	}
}

func TestExtractIdentifiers(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"fix TestBuildAgent", []string{"TestBuildAgent"}},
		{"What does BuildAgent do?", []string{"BuildAgent"}},
		{"refactor handleRequest and parseConfig", []string{"handleRequest", "parseConfig"}},
		{"This is a plain sentence", nil},
		{"look at ChatWithSession and BuildAll", []string{"ChatWithSession", "BuildAll"}},
	}
	for _, tt := range tests {
		got := extractIdentifiers(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("extractIdentifiers(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("extractIdentifiers(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestPredictiveContext_NilStore(t *testing.T) {
	buf := NewBuffer(10)
	result := PredictiveContext(context.Background(), nil, buf, "fix BuildAgent")
	if result != "" {
		t.Fatalf("expected empty for nil store, got %q", result)
	}
}

func TestPrefetch(t *testing.T) {
	store := setupTestStore(t)
	buf := NewBuffer(50)

	buf.Prefetch(context.Background(), store, "Caller")

	entry, ok := buf.Get("Caller")
	if !ok {
		t.Fatal("expected Caller in buffer after prefetch")
	}
	if entry.Symbol.Name != "Caller" {
		t.Fatalf("symbol name=%q, want Caller", entry.Symbol.Name)
	}
	if len(entry.Callees) == 0 {
		t.Fatal("expected callees for Caller")
	}
}

func TestMergeUnique(t *testing.T) {
	got := mergeUnique([]string{"a", "b", "c"}, []string{"b", "c", "d"})
	want := []string{"a", "b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func setupTestStore(t *testing.T) *graph.Store {
	t.Helper()
	store, err := graph.OpenStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()
	store.InsertSymbol(ctx, graph.Symbol{Name: "Caller", Kind: graph.KindFunc, Package: "main", File: "main.go", Line: 10, EndLine: 20})
	store.InsertSymbol(ctx, graph.Symbol{Name: "Callee", Kind: graph.KindFunc, Package: "main", File: "main.go", Line: 30, EndLine: 40})
	store.InsertSymbol(ctx, graph.Symbol{Name: "TestCaller", Kind: graph.KindFunc, Package: "main", File: "main_test.go", Line: 1, EndLine: 10})
	store.InsertEdge(ctx, graph.Edge{Kind: graph.EdgeCall, FromName: "Caller", ToName: "Callee"})
	store.InsertEdge(ctx, graph.Edge{Kind: graph.EdgeCall, FromName: "TestCaller", ToName: "Caller"})
	return store
}
