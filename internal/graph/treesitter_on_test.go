//go:build treesitter

package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIndexNonGoFilePython(t *testing.T) {
	dir := t.TempDir()
	src := "def foo(x):\n    return x\n\nclass Bar:\n    def method(self):\n        pass\n"
	path := filepath.Join(dir, "mod.py")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "mod.py")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols < 2 {
		t.Fatalf("expected at least 2 symbols (foo, Bar), got %d", symbols)
	}

	foo, err := store.FindSymbols(context.Background(), "foo", "")
	if err != nil || len(foo) == 0 {
		t.Fatalf("expected to find symbol foo, err=%v", err)
	}
	if foo[0].Kind != KindFunc {
		t.Errorf("foo kind = %s, want %s", foo[0].Kind, KindFunc)
	}

	bar, err := store.FindSymbols(context.Background(), "Bar", "")
	if err != nil || len(bar) == 0 {
		t.Fatalf("expected to find symbol Bar, err=%v", err)
	}
	if bar[0].Kind != KindStruct {
		t.Errorf("Bar kind = %s, want %s", bar[0].Kind, KindStruct)
	}
}

func TestIndexNonGoFileUnsupportedExtension(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	symbols, edges, err := IndexNonGoFile(context.Background(), store, dir, "readme.md")
	if err != nil || symbols != 0 || edges != 0 {
		t.Fatalf("expected (0, 0, nil) for unsupported extension, got (%d, %d, %v)", symbols, edges, err)
	}
}
