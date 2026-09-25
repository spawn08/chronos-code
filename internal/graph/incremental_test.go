package graph

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestAPIShapeBreaks(t *testing.T) {
	shape := func(src string) string {
		t.Helper()
		s, ok := goAPIShape([]byte(src))
		if !ok {
			t.Fatalf("goAPIShape(%q) failed", src)
		}
		return s
	}
	base := shape("package p\n\n// Doc.\nfunc F() int { return 1 }\n\ntype T struct{ A int }\n")
	cases := []struct {
		name   string
		src    string
		breaks bool
	}{
		{"body and comment edit", "package p\n\n// Other doc.\nfunc F() int {\n\tx := 2\n\treturn x\n}\n\ntype T struct{ A int }\n", false},
		{"added declaration", "package p\n\nfunc F() int { return 1 }\n\nfunc G() {}\n\ntype T struct{ A int }\n", false},
		{"signature change", "package p\n\nfunc F() string { return \"\" }\n\ntype T struct{ A int }\n", true},
		{"removed declaration", "package p\n\ntype T struct{ A int }\n", true},
		{"type change", "package p\n\nfunc F() int { return 1 }\n\ntype T struct{ A, B int }\n", true},
		{"build constraint", "//go:build linux\n\npackage p\n\nfunc F() int { return 1 }\n\ntype T struct{ A int }\n", true},
	}
	for _, tc := range cases {
		if got := apiShapeBreaks(base, shape(tc.src)); got != tc.breaks {
			t.Errorf("%s: apiShapeBreaks = %v, want %v", tc.name, got, tc.breaks)
		}
	}
	if apiShapeBreaks("", base) != true {
		t.Error("missing stored shape must be treated as a break")
	}
	if _, ok := goAPIShape([]byte("package p\n\nimport \"C\"\n")); ok {
		t.Error("cgo files must not report a shape")
	}
}

// Dependents are reloaded only when a dependency's declared API breaks, yet
// their facts stay correct across body edits, additions and breaking renames.
func TestIndexReloadsDependentsOnlyForAPIBreaks(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module tiny\n\ngo 1.24\n")
	write("lib/lib.go", "package lib\n\nfunc Helper() int { return 1 }\n")
	write("app/app.go", "package app\n\nimport \"tiny/lib\"\n\nfunc Use() int { return lib.Helper() }\n")

	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	ix := NewIndexer(store, root)
	index := func() *IndexStats {
		t.Helper()
		stats, err := ix.IndexAll(ctx)
		if err != nil {
			t.Fatalf("IndexAll: %v", err)
		}
		return stats
	}
	callers := func(name string) []string {
		t.Helper()
		got, err := store.CallersOf(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	index()
	if got := callers("Helper"); !slices.Equal(got, []string{"Use"}) {
		t.Fatalf("callers(Helper) = %v", got)
	}

	write("lib/lib.go", "package lib\n\nfunc Helper() int {\n\tx := 2\n\treturn x\n}\n")
	if stats := index(); stats.Packages != 1 {
		t.Fatalf("body edit reloaded %d packages, want only lib", stats.Packages)
	}
	write("lib/lib.go", "package lib\n\nfunc Helper() int { return 2 }\n\nfunc Extra() {}\n")
	if stats := index(); stats.Packages != 1 {
		t.Fatalf("additive edit reloaded %d packages, want only lib", stats.Packages)
	}
	if got := callers("Helper"); !slices.Equal(got, []string{"Use"}) {
		t.Fatalf("callers(Helper) after body edits = %v", got)
	}

	write("lib/lib.go", "package lib\n\nfunc Helper2() int { return 2 }\n")
	if stats := index(); stats.Packages != 2 {
		t.Fatalf("breaking rename reloaded %d packages, want lib and app", stats.Packages)
	}
	if got := callers("Helper"); len(got) != 0 {
		t.Fatalf("stale callers(Helper) after rename = %v", got)
	}
	write("app/app.go", "package app\n\nimport \"tiny/lib\"\n\nfunc Use() int { return lib.Helper2() }\n")
	index()
	if got := callers("Helper2"); !slices.Equal(got, []string{"Use"}) {
		t.Fatalf("callers(Helper2) = %v", got)
	}
}

// Calls made from methods are recorded under the receiver-qualified caller
// name, survive pruning, and are traversed by find_callers beyond depth 1.
func TestMethodCallersSurviveAndTraverse(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module tiny\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package tiny\n\ntype Svc struct{}\n\nfunc (s *Svc) Run() { helper() }\n\nfunc helper() {}\n\nfunc Main() { (&Svc{}).Run() }\n"
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := NewIndexer(store, root).IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.CallersOf(ctx, "helper"); !slices.Equal(got, []string{"Svc.Run"}) {
		t.Fatalf("callers(helper) = %v, want the method caller", got)
	}
	result, err := findCallersTool(store).Handler(ctx, map[string]any{"name": "helper", "depth": 2})
	if err != nil {
		t.Fatal(err)
	}
	levels := result.(map[string]any)["callers_by_depth"].([]map[string][]string)
	if len(levels) != 2 || !slices.Equal(levels[1]["Run"], []string{"Main"}) {
		t.Fatalf("depth-2 callers = %+v", levels)
	}
}
