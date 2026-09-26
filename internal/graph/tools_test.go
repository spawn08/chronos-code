package graph

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spawn08/chronos/engine/tool"
)

func TestToolsRegistration(t *testing.T) {
	defs := Tools(newMemBackend(), t.TempDir())
	want := []string{"graph_query", "find_callers", "find_implementations", "multi_resolution_view", "resolve_symbol", "codebase_search", "codebase_map", "codebase_context"}
	if len(defs) != len(want) {
		t.Fatalf("Tools returned %d definitions, want %d", len(defs), len(want))
	}
	for _, name := range want {
		if def := toolFrom(t, defs, name); def.Permission != tool.PermAllow {
			t.Fatalf("tool %q permission = %q, want %q", name, def.Permission, tool.PermAllow)
		}
	}
}

func TestCodebaseSearchTool(t *testing.T) {
	ctx := context.Background()
	store := newMemBackend()
	for _, symbol := range []Symbol{
		{Name: "Router", Kind: KindStruct, Package: "router", File: "router.go", Line: 1, EndLine: 2, Doc: "Routes coding tasks."},
		{Name: "SearchHelper", Kind: KindFunc, Package: "router", File: "search.go", Line: 1, EndLine: 2, Doc: "Uses the Router."},
	} {
		store.addSymbol(symbol)
	}

	var search *tool.Definition
	for _, def := range Tools(store, "") {
		if def.Name == "codebase_search" {
			search = def
			break
		}
	}
	if search == nil {
		t.Fatal("codebase_search is not registered")
	}

	out, err := search.Handler(ctx, map[string]any{"query": "Router", "top_k": 10})
	if err != nil {
		t.Fatalf("codebase_search: %v", err)
	}
	result := out.(map[string]any)
	symbols := result["symbols"].([]map[string]any)
	if len(symbols) != 2 {
		t.Fatalf("codebase_search symbols = %+v, want exact and text matches", symbols)
	}
	if symbols[0]["name"] != "Router" || symbols[1]["name"] != "SearchHelper" {
		t.Fatalf("codebase_search order = %+v, want exact Router before text match", symbols)
	}
	if _, err := search.Handler(ctx, map[string]any{"query": ""}); err == nil {
		t.Fatal("codebase_search accepted an empty query")
	}
	if _, err := search.Handler(ctx, map[string]any{"query": "router.go"}); err != nil {
		t.Fatalf("codebase_search dotted query: %v", err)
	}
}

func TestCodebaseMapTool(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := newMemBackend()

	packages := []struct {
		name   string
		file   string
		symbol Symbol
	}{
		{
			name: "example/auth",
			file: filepath.Join(root, "auth", "auth.go"),
			symbol: Symbol{Name: "Authenticator", Kind: KindStruct, Package: "example/auth", Line: 3,
				Signature: "type Authenticator struct{}", Doc: "Authenticates requests."},
		},
		{
			name: "example/other",
			file: filepath.Join(root, "other", "other.go"),
			symbol: Symbol{Name: "Other", Kind: KindStruct, Package: "example/other", Line: 4,
				Signature: "type Other struct{}", Doc: "Unrelated functionality."},
		},
	}
	for _, pkg := range packages {
		store.addPackage(pkg.name, "")
		store.addFile(pkg.file, pkg.name)
		pkg.symbol.File = pkg.file
		store.addSymbol(pkg.symbol)
	}
	store.addSymbol(Symbol{
		Name: "Authorizer", Kind: KindStruct, Package: packages[0].name, File: packages[0].file, Line: 8,
		Signature: "type Authorizer struct{}", Doc: "Authorizes requests.",
	})
	store.addPackage("example/authfallback", "")

	var codeMap *tool.Definition
	for _, def := range Tools(store, root) {
		if def.Name == "codebase_map" {
			codeMap = def
			break
		}
	}
	if codeMap == nil {
		t.Fatal("codebase_map is not registered")
	}
	if codeMap.Permission != tool.PermAllow {
		t.Fatalf("codebase_map permission = %q, want %q", codeMap.Permission, tool.PermAllow)
	}
	if _, required := codeMap.Parameters["required"]; required {
		t.Fatal("codebase_map query should be optional")
	}

	out, err := codeMap.Handler(ctx, map[string]any{"query": "requests"})
	if err != nil {
		t.Fatalf("codebase_map FTS query: %v", err)
	}
	got := out.(string)
	if strings.Count(got, "# Package `example/auth`") != 1 {
		t.Fatalf("codebase_map should deduplicate the ranked FTS package:\n%s", got)
	}
	if strings.Contains(got, "# Package `example/authfallback`") || strings.Contains(got, "# Package `example/other`") {
		t.Fatalf("codebase_map FTS query rendered an unrelated package:\n%s", got)
	}

	out, err = codeMap.Handler(ctx, map[string]any{"query": "auth"})
	if err != nil {
		t.Fatalf("codebase_map package fallback query: %v", err)
	}
	got = out.(string)
	if strings.Count(got, "# Package `example/auth`") != 1 {
		t.Fatalf("codebase_map should render each matched package once:\n%s", got)
	}
	if !strings.Contains(got, "# Package `example/authfallback`") {
		t.Fatalf("codebase_map should append package-name fallback matches:\n%s", got)
	}
	if strings.Contains(got, "# Package `example/other`") || strings.Contains(got, "# Code Map") {
		t.Fatalf("codebase_map query returned the full package index or an unrelated package:\n%s", got)
	}

	out, err = codeMap.Handler(ctx, nil)
	if err != nil {
		t.Fatalf("codebase_map index: %v", err)
	}
	index := out.(string)
	if !strings.Contains(index, "# Code Map") || !strings.Contains(index, "- `example/auth`") || !strings.Contains(index, "- `example/other`") {
		t.Fatalf("codebase_map empty query did not return the package index:\n%s", index)
	}
}

func TestCodebaseMapToolOutputByteLimit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := newMemBackend()

	pkg, file := "large", filepath.Join(root, "large.go")
	store.addPackage(pkg, "")
	store.addFile(file, pkg)
	store.addSymbol(Symbol{
		Name: "Large", Kind: KindFunc, Package: pkg, File: file, Line: 1,
		Signature: "func Large() " + strings.Repeat("界", codebaseMapMaxOutputBytes),
	})

	var codeMap *tool.Definition
	for _, def := range Tools(store, root) {
		if def.Name == "codebase_map" {
			codeMap = def
			break
		}
	}
	out, err := codeMap.Handler(ctx, map[string]any{"query": "Large"})
	if err != nil {
		t.Fatalf("codebase_map: %v", err)
	}
	got := out.(string)
	if len(got) > codebaseMapMaxOutputBytes {
		t.Fatalf("codebase_map output is %d bytes, limit is %d", len(got), codebaseMapMaxOutputBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("codebase_map byte limit split a UTF-8 encoding")
	}
	if !strings.HasSuffix(got, codebaseMapTruncationNotice) {
		t.Fatalf("codebase_map truncated output lacks notice: %q", got[len(got)-100:])
	}
}
