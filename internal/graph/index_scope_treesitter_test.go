//go:build treesitter

package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// In tree-sitter builds non-Go files keep answering through the SQLite tier,
// merged with the chronos index, and Go symbols are never duplicated even
// when the store still holds rows from an earlier IndexAll.
func TestIndexScopeMergesNonGoTier(t *testing.T) {
	root := canonicalTempDir(t)
	writeTree(t, root, payFiles)
	writeTree(t, root, map[string]string{"tools/report.py": "def build_report(rows):\n    return summarize(rows)\n\ndef summarize(rows):\n    return len(rows)\n"})
	db := filepath.Join(t.TempDir(), "graph.db")
	legacy, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewIndexer(legacy, root).IndexAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	s, err := NewIndexScope(context.Background(), IndexScopeOptions{Root: root, DataDir: t.TempDir(), GraphDB: db, IndexOnStart: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	query := toolFrom(t, s.Tools(), "graph_query")
	if r := call(t, ctx, query, map[string]any{"name": "build_report"}); r["found"] != true {
		t.Fatalf("python symbol: %+v", r)
	}
	r := call(t, ctx, query, map[string]any{"name": "Card"})
	if syms, _ := r["symbols"].([]map[string]any); len(syms) != 1 {
		t.Fatalf("Go symbol must come from the index only once: %+v", r)
	}
	c := call(t, ctx, toolFrom(t, s.Tools(), "find_callers"), map[string]any{"name": "summarize"})
	if levels := c["callers_by_depth"].([]map[string][]string); len(levels) == 0 || len(levels[0]["summarize"]) != 1 {
		t.Fatalf("python callers: %+v", c)
	}
	if err := os.Remove(filepath.Join(root, "tools", "report.py")); err != nil {
		t.Fatal(err)
	}
	s.nonGo.mu.Lock()
	s.nonGo.last = s.nonGo.last.Add(-nonGoRefreshInterval)
	s.nonGo.mu.Unlock()
	if r := call(t, ctx, query, map[string]any{"name": "build_report"}); r["found"] != false {
		t.Fatalf("deleted python file still indexed: %+v", r)
	}
}
