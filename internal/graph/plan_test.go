package graph

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestHotQueriesUseIndexes guards the query plans of the traversal hot paths:
// caller lookups must seek the covering edge index and caller declarations
// must seek the caller-identity expression index, never scan either table.
func TestHotQueriesUseIndexes(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.ReplaceFile(ctx, FileReplacement{Path: "/r/a.go", Package: "p", Hash: "h",
		Symbols: []Symbol{{Name: "Run", Kind: KindMethod, Receiver: "*Svc", Line: 1, EndLine: 3}, {Name: "helper", Kind: KindFunc, Line: 5, EndLine: 6}},
		Edges:   []Edge{{Kind: EdgeCall, FromName: "Svc.Run", ToName: "helper"}}}); err != nil {
		t.Fatal(err)
	}
	// Realistic statistics: many files, symbols and call edges.
	for f := 0; f < 60; f++ {
		var syms []Symbol
		var edges []Edge
		for i := 0; i < 20; i++ {
			name := fmt.Sprintf("F%d_%d", f, i)
			syms = append(syms, Symbol{Name: name, Kind: KindFunc, Line: i + 1, EndLine: i + 1})
			edges = append(edges, Edge{Kind: EdgeCall, FromName: name, ToName: fmt.Sprintf("F%d_%d", (f+1)%60, i)})
		}
		if err := store.ReplaceFile(ctx, FileReplacement{Path: fmt.Sprintf("/r/f%d.go", f), Package: "p", Hash: "h", Symbols: syms, Edges: edges}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Optimize(ctx); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		query string
		want  []string
		args  []any
	}{
		{`SELECT DISTINCT from_name FROM edges WHERE kind = ? AND to_name = ?`, []string{"idx_edges_to_from"}, []any{"call", "helper"}},
		{`SELECT DISTINCT to_name, from_name FROM edges WHERE kind = ? AND to_name IN (?, ?)`, []string{"idx_edges_to_from"}, []any{"call", "helper", "Run"}},
		{`SELECT DISTINCT to_name FROM edges WHERE kind = ? AND from_name = ?`, []string{"idx_edges_identity"}, []any{"call", "Svc.Run"}},
		{evidenceCallersQuery, []string{"idx_edges_to_from", "idx_symbols_caller"}, []any{"helper"}},
		{`SELECT id FROM symbols WHERE ` + symbolQualifiedExpr + ` IN (?, ?)`, []string{"idx_symbols_caller"}, []any{"Svc.Run", "helper"}},
	}
	for _, tc := range cases {
		rows, err := store.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+tc.query, tc.args...)
		if err != nil {
			t.Fatalf("explain %s: %v", tc.query, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, "\n")
		for _, index := range tc.want {
			if !strings.Contains(joined, "SEARCH") || !strings.Contains(joined, index) {
				t.Errorf("query %q does not seek %s:\n%s", tc.query, index, joined)
			}
		}
		if strings.Contains(joined, "SCAN edges") || strings.Contains(joined, "SCAN s ") || strings.Contains(joined, "SCAN symbols") {
			t.Errorf("query %q scans a table:\n%s", tc.query, joined)
		}
	}
	callers, err := store.CallersOfMany(ctx, []string{"helper"})
	if err != nil || len(callers["helper"]) != 1 || callers["helper"][0] != "Svc.Run" {
		t.Fatalf("CallersOfMany = %v, %v", callers, err)
	}
	if err := store.PruneStaleEdges(ctx); err != nil {
		t.Fatal(err)
	}
	if callers, _ := store.CallersOf(ctx, "helper"); len(callers) != 1 {
		t.Fatalf("PruneStaleEdges dropped a call made from a method: %v", callers)
	}
}
