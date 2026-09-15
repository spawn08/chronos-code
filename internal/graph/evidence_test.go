package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/model"
)

func evidenceFixture(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	for name, text := range map[string]string{
		"go.mod":          "module evidence\n\ngo 1.24\n",
		"service.go":      "package evidence\n// Charge records a payment.\nfunc Charge() {}\n",
		"handler.go":      "package evidence\nfunc Handle() { Charge() }\n",
		"service_test.go": "package evidence\nimport \"testing\"\nfunc TestHandle(t *testing.T) { Handle() }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := NewIndexer(store, root).IndexAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store, root
}

func evidenceCall(t *testing.T, s *Store, root string, args map[string]any) *evidenceResult {
	t.Helper()
	result, err := codebaseContextTool(s, root).Handler(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	return result.(*evidenceResult)
}

func TestEvidenceOneTurnCrossFile(t *testing.T) {
	s, root := evidenceFixture(t)
	before, err := s.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	def := codebaseContextTool(s, root)
	result, err := def.Handler(context.Background(), map[string]any{"query": "Charge", "max_tokens": 4096})
	if err != nil {
		t.Fatal(err)
	}
	got := result.(*evidenceResult)
	want := map[string]string{"definition": "Charge", "caller": "Handle", "test": "TestHandle"}
	for role, name := range want {
		found := false
		for _, item := range got.Items {
			if item.Role != role || item.Name != name {
				continue
			}
			found = true
			if item.File == "" || filepath.IsAbs(item.File) || item.StartLine < 1 || item.Source == nil || !strings.Contains(item.Source.Text, name) {
				t.Fatalf("incomplete %s evidence: %+v", role, item)
			}
			if item.Source.IndexedHash == "" || item.Source.Freshness != "verified" {
				t.Fatalf("missing verified revision: %+v", item.Source)
			}
			hash := sha256.Sum256([]byte(item.Source.Text))
			if item.Source.ExcerptSHA256 != hex.EncodeToString(hash[:]) {
				t.Fatal("excerpt revision does not describe returned bytes")
			}
		}
		if !found {
			t.Fatalf("missing %s %s in %+v", role, name, got)
		}
	}
	first, _ := json.Marshal(got)
	secondResult, err := def.Handler(context.Background(), map[string]any{"query": "Charge", "max_tokens": 4096})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := json.Marshal(secondResult)
	if string(first) != string(second) {
		t.Fatalf("nondeterministic result:\n%s\n%s", first, second)
	}
	after, err := s.Stats(context.Background())
	if err != nil || after != before {
		t.Fatalf("read-only tool mutated graph: %+v -> %+v, %v", before, after, err)
	}
	found := false
	for _, tool := range Tools(s, root) {
		if tool.Name == "codebase_context" {
			found = true
		}
	}
	if !found {
		t.Fatal("codebase_context not registered")
	}
	fts := evidenceCall(t, s, root, map[string]any{"query": "payment", "include": []any{"definitions"}})
	if len(fts.Items) != 1 || fts.Items[0].Name != "Charge" || fts.Items[0].IndexedHash == "" {
		t.Fatalf("FTS-only query lost definition/provenance: %+v", fts)
	}
}

func TestEvidenceSchemaIsAzureFunctionCompatible(t *testing.T) {
	s, root := evidenceFixture(t)
	parameters := codebaseContextTool(s, root).Parameters
	if parameters["type"] != "object" {
		t.Fatalf("schema type = %v, want object", parameters["type"])
	}
	for _, keyword := range []string{"oneOf", "anyOf", "allOf", "enum", "const", "not"} {
		if _, found := parameters[keyword]; found {
			t.Fatalf("Azure-incompatible top-level %s in schema", keyword)
		}
	}
	if _, err := codebaseContextTool(s, root).Handler(context.Background(), map[string]any{}); err == nil {
		t.Fatal("missing selector bypassed handler validation")
	}
}

func TestEvidenceMetadataOnlyBudgetAndConservativeFallback(t *testing.T) {
	s, root := evidenceFixture(t)
	for i := 0; i < 30; i++ {
		if err := s.InsertSymbol(context.Background(), Symbol{Name: "Charge", Kind: KindFunc, Package: strings.Repeat("metadata", 100), File: filepath.Join(root, "service.go"), Line: i + 1, EndLine: i + 1, Signature: strings.Repeat("signature ", 100)}); err != nil {
			t.Fatal(err)
		}
	}
	result := evidenceCall(t, s, root, map[string]any{"query": "Charge", "include": []any{"definitions"}, "max_tokens": 256})
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if n := model.NewTokenCounter("").CountString(string(data)); n+32+(n+9)/10 > 256 {
		t.Fatalf("metadata alone exceeds bound: %d", n)
	}
	if result.IOBytes != 0 || !result.Truncated || result.Omitted["selection_limit"] == 0 {
		t.Fatalf("metadata omissions not explicit: %+v", result)
	}
	result.MaxTokens = 512
	result.Counter = "sdk:bytes+10%+32"
	if err := fitEvidence(context.Background(), result, &model.EstimatingCounter{CharsPerToken: 1}); err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(result)
	if len(data)+1+32+(len(data)+10)/10 > 512 {
		t.Fatalf("conservative fallback exceeds bound: %s", data)
	}
}

func TestEvidenceFullSerializedBudget(t *testing.T) {
	s, root := evidenceFixture(t)
	text := "package evidence\n// " + strings.Repeat("复杂🧭 \\\" <>&\t abcDEF0123456789 ", 1000) + "\nfunc Charge() {}\n"
	if err := os.WriteFile(filepath.Join(root, "service.go"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int{256, 384, 512, 1024, 4096} {
		got := evidenceCall(t, s, root, map[string]any{"query": "Charge", "ranges": []any{map[string]any{"file": "service.go", "start_line": 2, "end_line": 3}}, "max_tokens": budget})
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		counter := model.NewTokenCounter("")
		tokens := counter.CountString(string(data))
		if tokens+32+(tokens+9)/10 > budget {
			t.Fatalf("full JSON costs %d before overhead, budget %d: %s", tokens, budget, data)
		}
		if !got.Truncated || len(got.Omitted) == 0 {
			t.Fatalf("missing explicit truncation for budget %d: %+v", budget, got)
		}
	}
}

func TestEvidenceBoundedReadsContainmentAndStaleness(t *testing.T) {
	s, root := evidenceFixture(t)
	path := filepath.Join(root, "large.go")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("package evidence\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got := evidenceCall(t, s, root, map[string]any{"ranges": []any{map[string]any{"file": "large.go", "start_line": 1, "end_line": 1}}, "include": []any{"excerpts"}})
	if got.IOBytes > 8192 || len(got.Items) != 1 || got.Items[0].Source.Text != "package evidence\n" {
		t.Fatalf("small excerpt performed excessive IO: %+v", got)
	}
	deep := evidenceCall(t, s, root, map[string]any{"ranges": []any{map[string]any{"file": "large.go", "start_line": 999999, "end_line": 1000000}}})
	if deep.IOBytes > evidenceReadLimit || !deep.Truncated {
		t.Fatalf("deep range not bounded: %+v", deep)
	}
	var spans []any
	for i := 0; i < 16; i++ {
		spans = append(spans, map[string]any{"file": "large.go", "start_line": 10000 + i, "end_line": 10000 + i})
	}
	aggregate := evidenceCall(t, s, root, map[string]any{"ranges": spans})
	if aggregate.IOBytes != evidenceReadLimit || aggregate.Omitted["read_limit"] == 0 {
		t.Fatalf("aggregate IO cap not enforced: %+v", aggregate)
	}
	if err := os.Symlink("service.go", filepath.Join(root, "inside.go")); err != nil {
		t.Fatal(err)
	}
	inside := evidenceCall(t, s, root, map[string]any{"ranges": []any{map[string]any{"file": "inside.go", "start_line": 1, "end_line": 1}}})
	if len(inside.Items) != 1 || inside.Items[0].Source.Text != "package evidence\n" {
		t.Fatalf("safe in-root symlink rejected: %+v", inside)
	}
	if err := os.WriteFile(filepath.Join(root, "unterminated.go"), []byte("package evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	unterminated := evidenceCall(t, s, root, map[string]any{"ranges": []any{map[string]any{"file": "unterminated.go", "start_line": 1, "end_line": 1}}})
	if unterminated.Omitted["range_unavailable"] != 0 || unterminated.Items[0].Source.EndLine != 1 {
		t.Fatalf("valid final line incorrectly omitted: %+v", unterminated)
	}
	outside := filepath.Join(t.TempDir(), "secret.go")
	if err := os.WriteFile(outside, []byte("DO_NOT_DISCLOSE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.go")); err != nil {
		t.Fatal(err)
	}
	blocked := evidenceCall(t, s, root, map[string]any{"ranges": []any{map[string]any{"file": "escape.go", "start_line": 1, "end_line": 1}}})
	data, _ := json.Marshal(blocked)
	if strings.Contains(string(data), "DO_NOT_DISCLOSE") || len(blocked.Omitted) == 0 {
		t.Fatalf("symlink escape not reported: %s", data)
	}
	if err := os.WriteFile(filepath.Join(root, "service.go"), []byte("package evidence\n// Changed since indexing.\nfunc Charge() { _ = 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := evidenceCall(t, s, root, map[string]any{"symbols": []any{"Charge"}})
	if len(stale.Items) == 0 || stale.Items[0].Source.Freshness != "stale" {
		t.Fatalf("stale graph not exposed: %+v", stale)
	}
	if err := os.Remove(filepath.Join(root, "service.go")); err != nil {
		t.Fatal(err)
	}
	missing := evidenceCall(t, s, root, map[string]any{"symbols": []any{"Charge"}})
	if missing.Omitted["missing_source"] == 0 {
		t.Fatalf("missing source not explicit: %+v", missing)
	}
}

func TestEvidenceValidationCancellationAndBatch(t *testing.T) {
	s, root := evidenceFixture(t)
	def := codebaseContextTool(s, root)
	for _, args := range []map[string]any{
		{}, {"query": 42}, {"query": "Charge", "max_tokens": 255}, {"query": "Charge", "max_tokens": 512.5},
		{"query": "Charge", "include": []any{"unknown"}}, {"symbols": []any{42}},
		{"ranges": []any{map[string]any{"file": "../secret", "start_line": 1, "end_line": 2}}},
		{"ranges": []any{map[string]any{"file": "service.go", "start_line": 3, "end_line": 2}}},
	} {
		if _, err := def.Handler(context.Background(), args); err == nil {
			t.Fatalf("accepted invalid arguments: %v", args)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := def.Handler(ctx, map[string]any{"query": "Charge"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	got := evidenceCall(t, s, root, map[string]any{"symbols": []any{"Charge", "Handle", "NotIndexed"}, "include": []any{"definitions"}, "max_tokens": 2048})
	if len(got.Items) != 2 || got.Omitted["not_found"] != 1 || got.IOBytes != 0 {
		t.Fatalf("batched definitions = %+v", got)
	}
	for _, item := range got.Items {
		if item.Source != nil {
			t.Fatal("read excerpts despite include filter")
		}
	}
}
