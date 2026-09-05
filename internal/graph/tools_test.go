package graph

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spawn08/chronos/engine/tool"
)

func TestToolsAgainstOwnRepo(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "graph.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(context.Background()); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}

	defs := Tools(store, root)
	byName := make(map[string]*tool.Definition)
	for _, d := range defs {
		byName[d.Name] = d
	}
	wantTools := []string{"graph_query", "find_callers", "find_implementations", "multi_resolution_view", "resolve_symbol", "codebase_search", "codebase_map"}
	if len(defs) != len(wantTools) {
		t.Fatalf("Tools returned %d definitions, want %d", len(defs), len(wantTools))
	}
	for _, want := range wantTools {
		def, ok := byName[want]
		if !ok {
			t.Fatalf("missing tool %q", want)
		}
		if def.Permission != tool.PermAllow {
			t.Fatalf("tool %q permission = %q, want %q", want, def.Permission, tool.PermAllow)
		}
	}

	ctx := context.Background()

	out, err := byName["graph_query"].Handler(ctx, map[string]any{"name": "OpenStore", "kind": "func"})
	if err != nil {
		t.Fatalf("graph_query: %v", err)
	}
	res := out.(map[string]any)
	if res["found"] != true {
		t.Fatalf("graph_query: expected to find OpenStore, got %+v", res)
	}

	out, err = byName["find_callers"].Handler(ctx, map[string]any{"name": "migrate"})
	if err != nil {
		t.Fatalf("find_callers: %v", err)
	}
	t.Logf("find_callers(migrate) = %+v", out)

	out, err = byName["multi_resolution_view"].Handler(ctx, map[string]any{"target": "", "level": "L0"})
	if err != nil {
		t.Fatalf("multi_resolution_view L0: %v", err)
	}
	l0 := out.(map[string]any)
	if len(l0["packages"].([]string)) == 0 {
		t.Fatal("multi_resolution_view L0: expected at least one package")
	}

	out, err = byName["resolve_symbol"].Handler(ctx, map[string]any{"name": "OpenStore"})
	if err != nil {
		t.Fatalf("resolve_symbol: %v", err)
	}
	t.Logf("resolve_symbol(OpenStore) = %+v", out)
}

func TestCodebaseSearchTool(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	for _, symbol := range []Symbol{
		{Name: "Router", Kind: KindStruct, Package: "router", File: "router.go", Line: 1, EndLine: 2, Doc: "Routes coding tasks."},
		{Name: "SearchHelper", Kind: KindFunc, Package: "router", File: "search.go", Line: 1, EndLine: 2, Doc: "Uses the Router."},
	} {
		if err := store.InsertSymbol(ctx, symbol); err != nil {
			t.Fatalf("InsertSymbol(%s): %v", symbol.Name, err)
		}
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
		t.Fatalf("codebase_search symbols = %+v, want exact and FTS matches", symbols)
	}
	if symbols[0]["name"] != "Router" || symbols[1]["name"] != "SearchHelper" {
		t.Fatalf("codebase_search order = %+v, want exact Router before FTS match", symbols)
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
	store := newCodeMapStore(t)
	defer store.Close()

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
		if err := store.UpsertPackage(ctx, pkg.name, ""); err != nil {
			t.Fatalf("UpsertPackage(%s): %v", pkg.name, err)
		}
		if err := store.UpsertFile(ctx, pkg.file, pkg.name, 1); err != nil {
			t.Fatalf("UpsertFile(%s): %v", pkg.file, err)
		}
		pkg.symbol.File = pkg.file
		if err := store.InsertSymbol(ctx, pkg.symbol); err != nil {
			t.Fatalf("InsertSymbol(%s): %v", pkg.symbol.Name, err)
		}
	}
	if err := store.InsertSymbol(ctx, Symbol{
		Name: "Authorizer", Kind: KindStruct, Package: packages[0].name, File: packages[0].file, Line: 8,
		Signature: "type Authorizer struct{}", Doc: "Authorizes requests.",
	}); err != nil {
		t.Fatalf("InsertSymbol(Authorizer): %v", err)
	}
	if err := store.UpsertPackage(ctx, "example/authfallback", ""); err != nil {
		t.Fatalf("UpsertPackage fallback: %v", err)
	}

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
	store := newCodeMapStore(t)
	defer store.Close()

	pkg, file := "large", filepath.Join(root, "large.go")
	if err := store.UpsertPackage(ctx, pkg, ""); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	if err := store.UpsertFile(ctx, file, pkg, 1); err != nil {
		t.Fatalf("UpsertFile: %v", err)
	}
	if err := store.InsertSymbol(ctx, Symbol{
		Name: "Large", Kind: KindFunc, Package: pkg, File: file, Line: 1,
		Signature: "func Large() " + strings.Repeat("界", codebaseMapMaxOutputBytes),
	}); err != nil {
		t.Fatalf("InsertSymbol: %v", err)
	}

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
