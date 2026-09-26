package activation

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/graph"
	"github.com/spawn08/chronos/engine/tool"
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
	t.Cleanup(func() { buf.Close() })

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

// A blocked graph query models a busy index without timing-dependent locks.
type blockingStore struct {
	*fakeGraph
	started chan string
	once    sync.Once
	exited  chan struct{}
}

func (s *blockingStore) FindSymbols(ctx context.Context, name, kind string) ([]graph.Symbol, error) {
	s.started <- name
	<-ctx.Done()
	s.once.Do(func() { close(s.exited) })
	return nil, ctx.Err()
}

func await(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for lifecycle completion")
	}
}

func TestBufferQueueBoundDedupAndClose(t *testing.T) {
	buf := NewBuffer(50)
	t.Cleanup(func() { buf.Close() })
	store := &blockingStore{started: make(chan string, 32), exited: make(chan struct{})}
	ctx := context.Background()
	first := buf.enqueue(ctx, store, "Active")
	if first == nil {
		t.Fatal("first job rejected")
	}
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if duplicate := buf.enqueue(ctx, store, "Active"); duplicate != first {
		t.Fatal("active duplicate was not coalesced")
	}
	for i := 1; i < maxPrefetchJobs; i++ {
		if buf.enqueue(ctx, store, fmt.Sprintf("Queued%d", i)) == nil {
			t.Fatalf("job %d unexpectedly rejected", i)
		}
	}
	if buf.enqueue(ctx, store, "Overflow") != nil {
		t.Fatal("queue exceeded hard bound")
	}
	if buf.enqueue(ctx, store, "Queued1") == nil {
		t.Fatal("queued duplicate rejected at capacity")
	}
	closed := make(chan struct{})
	go func() { buf.Close(); close(closed) }()
	await(t, closed)
	await(t, store.exited)
	await(t, first.done)
	buf.Close() // idempotent, including concurrent callers
	if buf.enqueue(ctx, store, "AfterClose") != nil {
		t.Fatal("accepted work after Close")
	}
	buf.mu.Lock()
	defer buf.mu.Unlock()
	if len(buf.pending) != 0 || len(buf.jobs) != 0 {
		t.Fatal("Close retained pending work")
	}
}

func TestBufferRequestCancellation(t *testing.T) {
	buf := NewBuffer(50)
	t.Cleanup(func() { buf.Close() })
	store := &blockingStore{started: make(chan string, 32), exited: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	job := buf.enqueue(ctx, store, "Active")
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	queuedCtx, queuedCancel := context.WithCancel(context.Background())
	queued := buf.enqueue(queuedCtx, store, "Queued")
	queuedCancel()
	cancel()
	await(t, job.done)
	await(t, queued.done)
	select {
	case name := <-store.started:
		t.Fatalf("canceled queued job queried store: %s", name)
	default:
	}
	if buf.enqueue(ctx, store, "Canceled") != nil {
		t.Fatal("accepted canceled request")
	}
}

func graphQuery(t *testing.T, store *fakeGraph) *tool.Definition {
	t.Helper()
	for _, def := range graph.Tools(store, "") {
		if def.Name == "graph_query" {
			return def
		}
	}
	t.Fatal("graph_query missing")
	return nil
}

func TestGraphQueryCacheKindRevisionAndCompleteness(t *testing.T) {
	ctx := context.Background()
	store := setupTestStore(t)
	buf := NewBuffer(50)
	t.Cleanup(func() { buf.Close() })
	if err := store.InsertSymbol(ctx, graph.Symbol{Name: "Caller", Kind: graph.KindType, Package: "other", File: "other.go"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFileHash(ctx, "main.go", "v1"); err != nil {
		t.Fatal(err)
	}
	buf.Prefetch(ctx, store, "Caller")
	original := graphQuery(t, store)
	wrapped := graphQuery(t, store)
	wrapGraphQuery(wrapped, store, buf)
	for _, kind := range []string{"", "func", "type", "method"} {
		args := map[string]any{"name": "Caller", "kind": kind}
		want, err := original.Handler(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		got, err := wrapped.Handler(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		m := got.(map[string]any)
		if kind != "method" && m["_activated"] != true {
			t.Fatalf("kind=%q: expected a validated cache hit", kind)
		}
		delete(m, "_activated")
		delete(m, "_neighbors")
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("kind=%q: got %#v want %#v", kind, got, want)
		}
	}
	// Stop asynchronous refresh so the stale/partial cache assertions are exact.
	buf.Close()
	if err := store.UpsertFileHash(ctx, "main.go", "v2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := buf.entriesForName(ctx, store, "Caller", "func"); ok {
		t.Fatal("stale revision was accepted")
	}
	if err := store.InsertSymbol(ctx, graph.Symbol{Name: "Caller", Kind: graph.KindType, Package: "new", File: "new.go"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := buf.entriesForName(ctx, store, "Caller", "type"); ok {
		t.Fatal("partial cached query was accepted")
	}
}

func TestPredictiveContextHardBoundsAndDeterminism(t *testing.T) {
	ctx := context.Background()
	store := setupTestStore(t)
	buf := NewBuffer(50)
	t.Cleanup(func() { buf.Close() })
	for i := 9; i >= 0; i-- {
		if err := store.InsertSymbol(ctx, graph.Symbol{Name: "Ambiguous", Kind: graph.KindFunc, File: fmt.Sprintf("%02d.go", i), Signature: strings.Repeat("界", 4000)}); err != nil {
			t.Fatal(err)
		}
	}
	got := PredictiveContext(ctx, store, buf, "Ambiguous Ambiguous Caller")
	if len(got) > maxPredictiveBytes || !utf8.ValidString(got) {
		t.Fatalf("invalid predictive byte budget: %d", len(got))
	}
	if count := strings.Count(got, "  Ambiguous"); count != maxPredictiveSymbols {
		t.Fatalf("summary count=%d", count)
	}
	if !strings.Contains(got, "00.go") || strings.Contains(got, "05.go") {
		t.Fatal("summary selection not deterministic")
	}
	if again := PredictiveContext(ctx, store, buf, "Ambiguous Ambiguous Caller"); again != got {
		t.Fatal("predictive output changed")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got := PredictiveContext(canceled, store, buf, "Caller"); got != "" {
		t.Fatalf("canceled context returned %q", got)
	}
}

type hashBlockingStore struct {
	*fakeGraph
	started chan struct{}
	release chan struct{}
}

func (s *hashBlockingStore) FileHash(ctx context.Context, file string) (string, error) {
	close(s.started)
	select {
	case <-s.release:
		return s.fakeGraph.FileHash(ctx, file)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestCacheValidationDoesNotHoldMutexDuringSQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	store := &hashBlockingStore{fakeGraph: setupTestStore(t), started: make(chan struct{}), release: make(chan struct{})}
	buf := NewBuffer(50)
	t.Cleanup(func() { buf.Close() })
	syms, err := store.FindSymbols(ctx, "Caller", "")
	if err != nil {
		t.Fatal(err)
	}
	key := entryKey(store, syms[0], "")
	buf.Put(key, &Entry{Symbol: syms[0], Repository: repositoryKey(store)})
	validated := make(chan struct{})
	go func() {
		defer close(validated)
		buf.entriesForName(ctx, store, "Caller", "func")
	}()
	await(t, store.started)
	cacheReady := make(chan struct{})
	go func() {
		buf.Put("Other", &Entry{Symbol: graph.Symbol{Name: "Other"}})
		buf.Get("Other")
		buf.Stats()
		close(cacheReady)
	}()
	select {
	case <-cacheReady:
	case <-time.After(time.Second):
		close(store.release)
		await(t, validated)
		t.Fatal("cache mutex held while FileHash was blocked")
	}
	close(store.release)
	await(t, validated)
}

func TestPrefetchQualifiedNeighborAndAmbiguousGet(t *testing.T) {
	ctx := context.Background()
	store := setupTestStore(t)
	buf := NewBuffer(50)
	t.Cleanup(func() { buf.Close() })
	buf.Prefetch(ctx, store, "Callee")
	before, ok := buf.Get("Callee")
	if !ok || len(before.Callers) == 0 {
		t.Fatal("missing deep callee entry")
	}
	buf.Prefetch(ctx, store, "Caller")
	after, ok := buf.Get("Callee")
	if !ok || before != after {
		t.Fatal("qualified neighbor was overwritten with a shallow entry")
	}
	buf.Put("one", &Entry{Symbol: graph.Symbol{Name: "Same", Package: "one"}})
	buf.Put("two", &Entry{Symbol: graph.Symbol{Name: "Same", Package: "two"}})
	if _, ok := buf.Get("Same"); ok {
		t.Fatal("ambiguous short name returned an arbitrary entry")
	}
}

func TestCacheUnknownRevisionAndRepositoryIsolation(t *testing.T) {
	ctx := context.Background()
	store := setupTestStore(t)
	other := setupTestStore(t)
	buf := NewBuffer(50)
	t.Cleanup(func() { buf.Close() })
	buf.Prefetch(ctx, store, "Caller")
	if _, ok := buf.entriesForName(ctx, other, "Caller", ""); ok {
		t.Fatal("cache crossed repositories")
	}
	if err := store.UpsertFileHash(ctx, "main.go", "first-revision"); err != nil {
		t.Fatal(err)
	}
	if _, ok := buf.entriesForName(ctx, store, "Caller", ""); ok {
		t.Fatal("unversioned entry survived a new revision")
	}
	buf.Prefetch(ctx, store, "Caller")
	if err := store.RemoveFile(ctx, "main.go"); err != nil {
		t.Fatal(err)
	}
	if _, ok := buf.entriesForName(ctx, store, "Caller", ""); ok {
		t.Fatal("deleted symbol returned from cache")
	}
}

func TestFindCallersUsesLiveEdgesAndDepth(t *testing.T) {
	ctx := context.Background()
	store := setupTestStore(t)
	buf := NewBuffer(50)
	t.Cleanup(func() { buf.Close() })
	buf.Prefetch(ctx, store, "Caller")
	if err := store.InsertCall(ctx, "NewCaller", "Caller"); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCall(ctx, "Outer", "NewCaller"); err != nil {
		t.Fatal(err)
	}
	var wrapped, original *tool.Definition
	for _, def := range graph.Tools(store, "") {
		if def.Name == "find_callers" {
			original = def
			copy := *def
			wrapped = &copy
		}
	}
	wrapFindCallers(wrapped, store, buf)
	for _, depth := range []any{nil, 1, int64(2), float64(2)} {
		args := map[string]any{"name": "Caller", "depth": depth}
		want, err := original.Handler(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		got, err := wrapped.Handler(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("depth=%v: got %#v want %#v", depth, got, want)
		}
	}
}

func TestBufferConcurrentCloseAndEnqueue(t *testing.T) {
	store := setupTestStore(t)
	buf := NewBuffer(50)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%3 == 0 {
				buf.Close()
				return
			}
			if i%3 == 1 {
				buf.Prefetch(context.Background(), store, "Caller")
				return
			}
			buf.Enqueue(context.Background(), store, "Caller")
		}(i)
	}
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	await(t, done)
	if buf.Enqueue(context.Background(), store, "Caller") {
		t.Fatal("accepted work after shutdown")
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

func setupTestStore(t *testing.T) *fakeGraph {
	t.Helper()
	ctx := context.Background()
	store := newFakeGraph()
	_ = store.InsertSymbol(ctx, graph.Symbol{Name: "Caller", Kind: graph.KindFunc, Package: "main", File: "main.go", Line: 10, EndLine: 20})
	_ = store.InsertSymbol(ctx, graph.Symbol{Name: "Callee", Kind: graph.KindFunc, Package: "main", File: "main.go", Line: 30, EndLine: 40})
	_ = store.InsertSymbol(ctx, graph.Symbol{Name: "TestCaller", Kind: graph.KindFunc, Package: "main", File: "main_test.go", Line: 1, EndLine: 10})
	_ = store.InsertCall(ctx, "Caller", "Callee")
	_ = store.InsertCall(ctx, "TestCaller", "Caller")
	return store
}
