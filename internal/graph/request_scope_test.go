package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
)

func TestRequestScopeIsolatesNestedWorkspaceAndRefreshesSources(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGraphWorkspace(t, parent, "parent.example/root", "ParentOnly")
	writeGraphWorkspace(t, child, "child.example/root", "ChildOnly")

	store, err := OpenStore(filepath.Join(t.TempDir(), "parent.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	if _, err := NewIndexer(store, parent).IndexAll(context.Background()); err != nil {
		t.Fatalf("index parent: %v", err)
	}

	scope := NewRequestScope(store, parent)
	defer scope.Close()
	query := requestToolNamed(t, scope.Tools(), "graph_query")

	assertGraphSymbol(t, query, context.Background(), "ParentOnly", true)
	childCtx := builtins.WithWorkspaceRoot(context.Background(), child)
	assertGraphSymbol(t, query, childCtx, "ChildOnly", true)
	assertGraphSymbol(t, query, childCtx, "ParentOnly", false)
	impact := requestToolNamed(t, scope.ImpactTools(), "impact_analysis")
	canonicalChild, err := filepath.EvalSymlinks(child)
	if err != nil {
		t.Fatal(err)
	}
	result, err := impact.Handler(childCtx, map[string]any{"file": filepath.Join(canonicalChild, "main.go"), "start_line": 3, "end_line": 3})
	if err != nil {
		t.Fatalf("impact_analysis: %v", err)
	}
	affected := result.(map[string]any)["affected_symbols"].([]map[string]any)
	if len(affected) != 1 || affected[0]["symbol"] != "ChildOnly" {
		t.Fatalf("child impact_analysis leaked parent main.go: %+v", affected)
	}

	if err := os.WriteFile(filepath.Join(child, "go.mod"), []byte("module child.example/fresh\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	childResult := queryGraphSymbol(t, query, childCtx, "ChildOnly")
	symbols := childResult["symbols"].([]map[string]any)
	if len(symbols) != 1 || symbols[0]["package"] != "child.example/fresh" {
		t.Fatalf("graph was not refreshed after config change: %+v", childResult)
	}

	writeGraphSource(t, child, "ChildFresh")
	assertGraphSymbol(t, query, childCtx, "ChildFresh", true)
	assertGraphSymbol(t, query, childCtx, "ChildOnly", false)
}

func TestRequestScopeCanonicalizesWorkspaceIdentity(t *testing.T) {
	root := t.TempDir()
	writeGraphWorkspace(t, root, "canonical.example/root", "Canonical")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	scope := NewRequestScope(nil, root)
	defer scope.Close()
	query := requestToolNamed(t, scope.Tools(), "graph_query")
	assertGraphSymbol(t, query, builtins.WithWorkspaceRoot(context.Background(), root), "Canonical", true)
	assertGraphSymbol(t, query, builtins.WithWorkspaceRoot(context.Background(), alias), "Canonical", true)
	if len(scope.entries) != 1 {
		t.Fatalf("canonical aliases created %d graph entries, want 1", len(scope.entries))
	}
}

func TestRequestScopeReturnsExplicitUnavailableResult(t *testing.T) {
	scope := NewRequestScope(nil, t.TempDir())
	defer scope.Close()
	query := requestToolNamed(t, scope.Tools(), "graph_query")
	missing := filepath.Join(t.TempDir(), "missing")
	result, err := query.Handler(builtins.WithWorkspaceRoot(context.Background(), missing), map[string]any{"name": "Anything"})
	if err != nil {
		t.Fatalf("graph_query: %v", err)
	}
	unavailable := result.(map[string]any)
	if unavailable["available"] != false || unavailable["workspace_root"] != missing || unavailable["reason"] == "" {
		t.Fatalf("unexpected unavailable result: %+v", unavailable)
	}
}

func writeGraphWorkspace(t *testing.T, root, module, symbol string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+module+"\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeGraphSource(t, root, symbol)
}

func writeGraphSource(t *testing.T, root, symbol string) {
	t.Helper()
	source := "package sample\n\nfunc " + symbol + "() {}\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requestToolNamed(t *testing.T, definitions []*tool.Definition, name string) *tool.Definition {
	t.Helper()
	for _, definition := range definitions {
		if definition.Name == name {
			return definition
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func assertGraphSymbol(t *testing.T, definition *tool.Definition, ctx context.Context, name string, want bool) {
	t.Helper()
	response := queryGraphSymbol(t, definition, ctx, name)
	if response["found"] != want {
		t.Fatalf("graph_query(%s) = %+v, want found=%v", name, response, want)
	}
}

func queryGraphSymbol(t *testing.T, definition *tool.Definition, ctx context.Context, name string) map[string]any {
	t.Helper()
	result, err := definition.Handler(ctx, map[string]any{"name": name})
	if err != nil {
		t.Fatalf("graph_query(%s): %v", name, err)
	}
	return result.(map[string]any)
}
