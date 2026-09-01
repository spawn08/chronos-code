package graph

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderCodeMapIncludesFilesImportsAndSignatures(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := newCodeMapStore(t)
	defer store.Close()

	files := []string{
		filepath.Join(root, "pkg", "empty.go"),
		filepath.Join(root, "pkg", "code.go"),
	}
	for _, file := range files {
		if err := store.UpsertFile(ctx, file, "example/pkg", 1); err != nil {
			t.Fatalf("UpsertFile: %v", err)
		}
	}
	if err := store.UpsertPackage(ctx, "example/pkg", "fmt,encoding/json,fmt"); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	for _, symbol := range []Symbol{
		{Name: "Beta", Kind: KindType, Package: "example/pkg", File: files[1], Line: 8, EndLine: 8, Signature: "type Beta string"},
		{Name: "Alpha", Kind: KindFunc, Package: "example/pkg", File: files[1], Line: 3, EndLine: 5, Signature: "func Alpha() error"},
		{Name: "Alpha", Kind: KindFunc, Package: "example/pkg", File: files[1], Line: 3, EndLine: 5, Signature: "func Alpha() error"},
	} {
		if err := store.InsertSymbol(ctx, symbol); err != nil {
			t.Fatalf("InsertSymbol: %v", err)
		}
	}

	got, err := RenderCodeMap(ctx, store, root, "example/pkg")
	if err != nil {
		t.Fatalf("RenderCodeMap: %v", err)
	}
	want := "# Package `example/pkg`\n\n" +
		"## Imports\n" +
		"- `encoding/json`\n" +
		"- `fmt`\n\n" +
		"## Files\n" +
		"### [`pkg/code.go`](pkg/code.go)\n" +
		"- `func Alpha() error` (func, line 3)\n" +
		"- `type Beta string` (type, line 8)\n\n" +
		"### [`pkg/empty.go`](pkg/empty.go)\n" +
		"_No indexed symbols._\n"
	if got != want {
		t.Fatalf("RenderCodeMap output:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderCodeMapDeterministicAcrossInsertionOrder(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	paths := []string{filepath.Join(root, "b.go"), filepath.Join(root, "a.go")}

	render := func(reverse bool) string {
		store := newCodeMapStore(t)
		defer store.Close()
		if err := store.UpsertPackage(ctx, "pkg", "z.example/a,a.example/z,z.example/a"); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
		for i := range paths {
			index := i
			if reverse {
				index = len(paths) - 1 - i
			}
			if err := store.UpsertFile(ctx, paths[index], "pkg", 1); err != nil {
				t.Fatalf("UpsertFile: %v", err)
			}
		}
		symbols := []Symbol{
			{Name: "Zed", Kind: KindFunc, Package: "pkg", File: paths[0], Line: 9, Signature: "func Zed()"},
			{Name: "Able", Kind: KindFunc, Package: "pkg", File: paths[1], Line: 2, Signature: "func Able()"},
		}
		for i := range symbols {
			index := i
			if reverse {
				index = len(symbols) - 1 - i
			}
			if err := store.InsertSymbol(ctx, symbols[index]); err != nil {
				t.Fatalf("InsertSymbol: %v", err)
			}
		}
		got, err := RenderCodeMap(ctx, store, root, "pkg")
		if err != nil {
			t.Fatalf("RenderCodeMap: %v", err)
		}
		return got
	}

	first, second := render(false), render(true)
	if first != second {
		t.Fatalf("rendering depends on insertion order:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestRenderCodeMapNormalizesSafePaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := newCodeMapStore(t)
	defer store.Close()

	inRoot := filepath.Join(root, "pkg", "safe name.go")
	outOfRoot := filepath.Join(filepath.Dir(root), "secret.go")
	unsafeRelative := filepath.Join("..", "escape.go")
	for _, path := range []string{inRoot, "pkg/relative.go", outOfRoot, unsafeRelative} {
		if err := store.UpsertFile(ctx, path, "pkg", 1); err != nil {
			t.Fatalf("UpsertFile(%s): %v", path, err)
		}
	}

	got, err := RenderCodeMap(ctx, store, root, "pkg")
	if err != nil {
		t.Fatalf("RenderCodeMap: %v", err)
	}
	if !strings.Contains(got, "[`pkg/safe name.go`](pkg/safe%20name.go)") {
		t.Fatalf("in-root path was not normalized safely:\n%s", got)
	}
	if !strings.Contains(got, "[`pkg/relative.go`](pkg/relative.go)") {
		t.Fatalf("relative path was not preserved as a safe workspace link:\n%s", got)
	}
	if strings.Contains(got, "](../") || strings.Contains(got, outOfRoot) {
		t.Fatalf("output contains an unsafe or absolute out-of-root path:\n%s", got)
	}
	if !strings.Contains(got, "`secret.go` (outside workspace)") || !strings.Contains(got, "`escape.go` (outside workspace)") {
		t.Fatalf("out-of-root files should remain visible without links:\n%s", got)
	}
}

func TestRenderCodeMapEmptyPackage(t *testing.T) {
	ctx := context.Background()
	store := newCodeMapStore(t)
	defer store.Close()

	got, err := RenderCodeMap(ctx, store, t.TempDir(), "empty")
	if err != nil {
		t.Fatalf("RenderCodeMap: %v", err)
	}
	want := "# Package `empty`\n\n## Imports\n_None._\n\n## Files\n_None._\n"
	if got != want {
		t.Fatalf("RenderCodeMap empty output = %q, want %q", got, want)
	}
}

func TestRenderCodeMapIndex(t *testing.T) {
	ctx := context.Background()
	store := newCodeMapStore(t)
	defer store.Close()
	for _, pkg := range []string{"z.example/pkg", "a.example/pkg"} {
		if err := store.UpsertPackage(ctx, pkg, ""); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
	}

	got, err := RenderCodeMapIndex(ctx, store)
	if err != nil {
		t.Fatalf("RenderCodeMapIndex: %v", err)
	}
	want := "# Code Map\n\n## Packages\n- `a.example/pkg`\n- `z.example/pkg`\n"
	if got != want {
		t.Fatalf("RenderCodeMapIndex output = %q, want %q", got, want)
	}

	empty := newCodeMapStore(t)
	defer empty.Close()
	got, err = RenderCodeMapIndex(ctx, empty)
	if err != nil {
		t.Fatalf("RenderCodeMapIndex empty: %v", err)
	}
	want = "# Code Map\n\n## Packages\n_No indexed packages._\n"
	if got != want {
		t.Fatalf("RenderCodeMapIndex empty output = %q, want %q", got, want)
	}
}

func newCodeMapStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return store
}
