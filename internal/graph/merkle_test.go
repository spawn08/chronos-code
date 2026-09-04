package graph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileHash_Deterministic(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	content := []byte("package x\n\nfunc F() {}\n")
	if err := os.WriteFile(a, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, content, 0o644); err != nil {
		t.Fatal(err)
	}

	ha, err := FileHash(a)
	if err != nil {
		t.Fatalf("FileHash(a): %v", err)
	}
	hb, err := FileHash(b)
	if err != nil {
		t.Fatalf("FileHash(b): %v", err)
	}
	if ha != hb {
		t.Errorf("FileHash of identical content differs: %q vs %q", ha, hb)
	}

	if err := os.WriteFile(b, []byte("package x\n\nfunc G() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hb2, err := FileHash(b)
	if err != nil {
		t.Fatalf("FileHash(b) after edit: %v", err)
	}
	if hb2 == hb {
		t.Error("FileHash did not change after editing file content")
	}
}

func TestDiffTree(t *testing.T) {
	dir := t.TempDir()
	writeGo := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeGo("a.go", "package x\n\nfunc A() {}\n")
	writeGo("b.go", "package x\n\nfunc B() {}\n")

	oldTree, err := BuildTree(dir, map[string]bool{".go": true})
	if err != nil {
		t.Fatalf("BuildTree (old): %v", err)
	}

	// Modify a.go, delete b.go, add c.go.
	writeGo("a.go", "package x\n\nfunc A() { _ = 1 }\n")
	if err := os.Remove(filepath.Join(dir, "b.go")); err != nil {
		t.Fatal(err)
	}
	writeGo("c.go", "package x\n\nfunc C() {}\n")

	newTree, err := BuildTree(dir, map[string]bool{".go": true})
	if err != nil {
		t.Fatalf("BuildTree (new): %v", err)
	}

	changed := DiffTree(oldTree, newTree)
	changedSet := make(map[string]bool, len(changed))
	for _, p := range changed {
		changedSet[p] = true
	}

	aPath := filepath.Join(dir, "a.go")
	bPath := filepath.Join(dir, "b.go")
	cPath := filepath.Join(dir, "c.go")

	if !changedSet[aPath] {
		t.Errorf("DiffTree missed modified file %s", aPath)
	}
	if !changedSet[cPath] {
		t.Errorf("DiffTree missed added file %s", cPath)
	}
	if changedSet[bPath] {
		t.Errorf("DiffTree reported deleted file %s as changed; DiffTree only reports additions/modifications present in newTree", bPath)
	}
	if _, stillPresent := newTree.Files[bPath]; stillPresent {
		t.Errorf("newTree should not contain removed file %s", bPath)
	}
}

// TestIndexAll_SkipsUnchangedFiles covers AC-4.2: a second IndexAll pass
// over an unchanged tree re-parses zero files.
func TestIndexAll_SkipsUnchangedFiles(t *testing.T) {
	root := newTinyModule(t)
	dbPath := filepath.Join(t.TempDir(), "graph.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	ix := NewIndexer(store, root)
	first, err := ix.IndexAll(context.Background())
	if err != nil {
		t.Fatalf("first IndexAll: %v", err)
	}
	if first.Files == 0 {
		t.Fatal("expected the first pass to index at least one file")
	}

	second, err := ix.IndexAll(context.Background())
	if err != nil {
		t.Fatalf("second IndexAll: %v", err)
	}
	if second.Files != 0 {
		t.Errorf("second IndexAll.Files = %d, want 0 (nothing changed)", second.Files)
	}
	if second.Skipped == 0 {
		t.Error("second IndexAll.Skipped = 0, want every file skipped as unchanged")
	}
}

// TestIndexAll_ReindexesChangedFile covers AC-4.1/AC-4.3's IndexAll half:
// after editing one file, only that file (not the whole tree) is
// re-parsed, and every row still carries a non-empty content_hash.
func TestIndexAll_ReindexesChangedFile(t *testing.T) {
	root := newTinyModule(t)
	dbPath := filepath.Join(t.TempDir(), "graph.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatalf("first IndexAll: %v", err)
	}

	hashes, err := store.AllFileHashes(ctx)
	if err != nil {
		t.Fatalf("AllFileHashes: %v", err)
	}
	if len(hashes) == 0 {
		t.Fatal("expected non-empty content_hash for every indexed file (AC-4.1)")
	}

	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package tiny\n\nfunc A() { _ = 2 }\n\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := ix.IndexAll(ctx)
	if err != nil {
		t.Fatalf("second IndexAll: %v", err)
	}
	if second.Files != 1 {
		t.Errorf("second IndexAll.Files = %d, want 1 (only a.go changed)", second.Files)
	}
}

// TestIndexFile_UnchangedShortCircuits covers AC-4.3's watcher-facing half:
// calling IndexFile again for a file whose content hasn't changed returns
// immediately with zero stats, without needing a second full IndexAll.
func TestIndexFile_UnchangedShortCircuits(t *testing.T) {
	root := newTinyModule(t)
	dbPath := filepath.Join(t.TempDir(), "graph.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}

	aPath := filepath.Join(root, "a.go")
	stats, err := ix.IndexFile(ctx, aPath)
	if err != nil {
		t.Fatalf("IndexFile (unchanged): %v", err)
	}
	if stats.Files != 0 || stats.Packages != 0 || stats.Symbols != 0 {
		t.Errorf("IndexFile on an unchanged file returned %+v, want all-zero stats", stats)
	}
}

// TestIndexFile_ScopedToPackage covers AC-4.3: editing one file and calling
// IndexFile re-parses only that file, not the rest of the (single-package)
// tiny module — and not the whole repo.
func TestIndexFile_ScopedToPackage(t *testing.T) {
	root := newTinyModule(t)
	dbPath := filepath.Join(t.TempDir(), "graph.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}

	aPath := filepath.Join(root, "a.go")
	if err := os.WriteFile(aPath, []byte("package tiny\n\nfunc A() { _ = 3 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stats, err := ix.IndexFile(ctx, aPath)
	if err != nil {
		t.Fatalf("IndexFile (changed): %v", err)
	}
	if stats.Files != 1 {
		t.Errorf("IndexFile.Files = %d, want 1 (only a.go changed; b.go in the same package must be skipped via its unchanged hash)", stats.Files)
	}
	if stats.Skipped == 0 {
		t.Error("IndexFile.Skipped = 0, want b.go counted as skipped (unchanged hash within the reindexed package)")
	}
}

func TestIndexFile_RemovesStaleCallEdge(t *testing.T) {
	root := newTinyModule(t)
	aPath := filepath.Join(root, "a.go")
	if err := os.WriteFile(aPath, []byte("package tiny\n\nfunc A() { B() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}
	if err := os.WriteFile(aPath, []byte("package tiny\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IndexFile(ctx, aPath); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}
	callers, err := store.CallersOf(ctx, "B")
	if err != nil {
		t.Fatalf("CallersOf: %v", err)
	}
	if len(callers) != 0 {
		t.Fatalf("CallersOf(B) = %v, want no stale callers", callers)
	}
}

func TestIndexAll_DeletesFileAndItsEdges(t *testing.T) {
	root := newTinyModule(t)
	aPath := filepath.Join(root, "a.go")
	if err := os.WriteFile(aPath, []byte("package tiny\n\nfunc A() { B() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}
	if err := os.Remove(aPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatalf("IndexAll after delete: %v", err)
	}
	if symbols, err := store.SymbolsInFile(ctx, aPath); err != nil || len(symbols) != 0 {
		t.Fatalf("SymbolsInFile(deleted) = %v, %v; want empty", symbols, err)
	}
	if callers, err := store.CallersOf(ctx, "B"); err != nil || len(callers) != 0 {
		t.Fatalf("CallersOf(B) after delete = %v, %v; want empty", callers, err)
	}
}

func TestIndexAll_ReindexDoesNotDuplicateEdges(t *testing.T) {
	root := newTinyModule(t)
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package tiny\n\nfunc A() { B() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatalf("first IndexAll: %v", err)
	}
	first, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("first Stats: %v", err)
	}
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatalf("second IndexAll: %v", err)
	}
	second, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("second Stats: %v", err)
	}
	if second.Edges != first.Edges {
		t.Fatalf("edge count after unchanged reindex = %d, want %d", second.Edges, first.Edges)
	}
}

func TestIndexFile_PreservesDuplicateCallerNameEdges(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module duplicate\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"one", "two"} {
		dir := filepath.Join(root, pkg)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package "+pkg+"\n\nfunc Caller() { Target() }\nfunc Target() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}
	onePath := filepath.Join(root, "one", "main.go")
	if err := os.WriteFile(onePath, []byte("package one\n\nfunc Caller() {}\nfunc Target() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IndexFile(ctx, onePath); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}
	var edges int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE kind = ? AND from_name = ? AND to_name = ?`, string(EdgeCall), "Caller", "Target").Scan(&edges); err != nil {
		t.Fatalf("count duplicate caller edges: %v", err)
	}
	if edges != 1 {
		t.Fatalf("Caller -> Target edge count = %d, want 1 from the unchanged package", edges)
	}
}

// newTinyModule writes a minimal two-file Go module to a temp dir and
// returns its root, for fast functional tests that don't need a real repo.
func newTinyModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module tiny\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package tiny\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("package tiny\n\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// BenchmarkIndexFile covers AC-4.4: a single-file incremental re-index
// completes in <100ms on a 50K-LOC Go repo. It generates a synthetic
// module of 200 independent, single-file packages (~250 lines each, ~50K
// LOC total) rather than mutating a real checkout, so the benchmark can't
// leave a sibling repo's working tree dirty. The packages import nothing
// from each other, so IndexFile's scoped packages.Load(Dir: pkgDir, ".")
// only ever has to load the one package being edited — this benchmark is
// exactly what demonstrates that its cost is bounded by package size, not
// total repo size.
func BenchmarkIndexFile(b *testing.B) {
	root := generateSyntheticRepo(b, 200, 250)
	dbPath := filepath.Join(b.TempDir(), "graph.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		b.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		b.Fatalf("IndexAll: %v", err)
	}

	target := filepath.Join(root, "pkg000", "pkg000.go")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := os.WriteFile(target, []byte(syntheticPackageBody("pkg000", 250, i)), 0o644); err != nil {
			b.Fatalf("write %s: %v", target, err)
		}
		if _, err := ix.IndexFile(ctx, target); err != nil {
			b.Fatalf("IndexFile: %v", err)
		}
	}
}

// generateSyntheticRepo writes numPkgs independent single-file packages of
// roughly linesPerPkg lines each to a temp module and returns its root.
func generateSyntheticRepo(b *testing.B, numPkgs, linesPerPkg int) string {
	b.Helper()
	dir := b.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module synthetic\n\ngo 1.24\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < numPkgs; i++ {
		name := fmt.Sprintf("pkg%03d", i)
		pkgDir := filepath.Join(dir, name)
		if err := os.MkdirAll(pkgDir, 0o755); err != nil {
			b.Fatal(err)
		}
		body := syntheticPackageBody(name, linesPerPkg, -1)
		if err := os.WriteFile(filepath.Join(pkgDir, name+".go"), []byte(body), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	return dir
}

// syntheticPackageBody generates a single-file package body with roughly
// lines trivial functions. version is folded into the body so successive
// calls with different values always produce different content (and thus
// a different content hash) for the same package name.
func syntheticPackageBody(pkgName string, lines, version int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n// version %d\n\n", pkgName, version)
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "func F%d() int { return %d }\n\n", i, i+version)
	}
	return b.String()
}
