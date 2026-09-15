package graph

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestIndexIncludesTestsWithoutDuplicateVariants(t *testing.T) {
	root := newTinyModule(t)
	for path, source := range map[string]string{
		"a_test.go":        "package tiny\nimport \"testing\"\nfunc TestInternal(t *testing.T) { A(); panic(\"indexing must not execute tests\") }\n",
		"external_test.go": "package tiny_test\nimport (\"testing\"; \"tiny\")\nfunc TestExternal(t *testing.T) { tiny.A() }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	ix := NewIndexer(store, root)
	stats, err := ix.IndexAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 4 || stats.Packages != 2 {
		t.Fatalf("stats = %+v; want 4 source files, 2 canonical packages", stats)
	}
	for _, name := range []string{"A", "B", "TestInternal", "TestExternal"} {
		syms, err := store.FindSymbols(ctx, name, "")
		if err != nil || len(syms) != 1 {
			t.Fatalf("%s symbols = %v, %v", name, syms, err)
		}
	}
	callers, err := store.CallersOf(ctx, "A")
	if err != nil || len(callers) != 2 {
		t.Fatalf("test callers = %v, %v", callers, err)
	}
	stats, err = ix.IndexAll(ctx)
	if err != nil || stats.Files != 0 || stats.Skipped != 4 {
		t.Fatalf("unchanged pass = %+v, %v", stats, err)
	}
	path := filepath.Join(root, "external_test.go")
	if err := os.WriteFile(path, []byte("package tiny_test\nimport \"testing\"\nfunc TestExternal(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IndexFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	callers, err = store.CallersOf(ctx, "A")
	if err != nil || !reflect.DeepEqual(callers, []string{"TestInternal"}) {
		t.Fatalf("remaining test callers = %v, %v", callers, err)
	}
}

func TestIndexFileRollbackAndRetry(t *testing.T) {
	root := newTinyModule(t)
	path := filepath.Join(root, "a.go")
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	oldHash, err := store.FileHash(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	oldSymbols, err := store.SymbolsInFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package tiny\nfunc Changed() { B() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_edge BEFORE INSERT ON edges BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IndexFile(ctx, path); err == nil {
		t.Fatal("expected edge insertion failure")
	}
	if hash, err := store.FileHash(ctx, path); err != nil || hash != oldHash {
		t.Fatalf("hash = %q, %v", hash, err)
	}
	if syms, err := store.SymbolsInFile(ctx, path); err != nil || !reflect.DeepEqual(syms, oldSymbols) {
		t.Fatalf("symbols = %v, %v", syms, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_edge`); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IndexFile(ctx, path); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if callers, err := store.CallersOf(ctx, "B"); err != nil || !reflect.DeepEqual(callers, []string{"Changed"}) {
		t.Fatalf("retry callers = %v, %v", callers, err)
	}
	newHash, err := store.FileHash(ctx, path)
	if err != nil || newHash == oldHash {
		t.Fatalf("retry hash = %q, %v", newHash, err)
	}
	if err := os.WriteFile(path, []byte("package tiny\nfunc Broken("), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IndexFile(ctx, path); err == nil {
		t.Fatal("expected syntax failure")
	}
	if hash, err := store.FileHash(ctx, path); err != nil || hash != newHash {
		t.Fatalf("syntax failure hash = %q, %v", hash, err)
	}
}

func TestIndexInvalidatesUnchangedImplementationOwner(t *testing.T) {
	root := newTinyModule(t)
	for path, source := range map[string]string{
		"a.go": "package tiny\ntype Concrete struct{}\n",
		"b.go": "package tiny\ntype Contract interface { Run() }\nfunc (Concrete) Run() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	var source string
	if err := store.db.QueryRow(`SELECT source_file FROM edges WHERE kind = 'implements' AND from_name = 'Concrete' AND to_name = 'Contract'`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != filepath.Join(root, "a.go") {
		t.Fatalf("implements owner = %s", source)
	}
	path := filepath.Join(root, "b.go")
	if err := os.WriteFile(path, []byte("package tiny\ntype Contract interface { Run() }\nfunc (Concrete) Stop() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stats, err := ix.IndexFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 2 {
		t.Fatalf("stats = %+v; want changed method file plus unchanged semantic owner", stats)
	}
	if impls, err := store.ImplementationsOf(ctx, "Contract"); err != nil || len(impls) != 0 {
		t.Fatalf("stale implementations = %v, %v", impls, err)
	}
	if err := os.WriteFile(path, []byte("package tiny\ntype Contract interface { Stop() }\nfunc (Concrete) Stop() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	if impls, err := store.ImplementationsOf(ctx, "Contract"); err != nil || !reflect.DeepEqual(impls, []string{"Concrete"}) {
		t.Fatalf("new implementations = %v, %v", impls, err)
	}
}

func TestIndexScopedPreservesExternalInterfacesUntilFullRefresh(t *testing.T) {
	root := newTinyModule(t)
	dir := filepath.Join(root, "contract")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ifacePath := filepath.Join(dir, "contract.go")
	if err := os.WriteFile(ifacePath, []byte("package contract\ntype Contract interface { Run() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "a.go")
	if err := os.WriteFile(path, []byte("package tiny\ntype Concrete struct{}\nfunc (Concrete) Run() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	ix := NewIndexer(store, root)
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package tiny\ntype Concrete struct{}\nfunc (Concrete) Run() { _ = 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IndexFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	if impls, err := store.ImplementationsOf(ctx, "Contract"); err != nil || !reflect.DeepEqual(impls, []string{"Concrete"}) {
		t.Fatalf("scoped pass lost external relationship: %v, %v", impls, err)
	}
	if err := os.WriteFile(ifacePath, []byte("package contract\ntype Contract interface { Stop() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IndexAll(ctx); err != nil {
		t.Fatal(err)
	}
	if impls, err := store.ImplementationsOf(ctx, "Contract"); err != nil || len(impls) != 0 {
		t.Fatalf("full pass retained stale external relationship: %v, %v", impls, err)
	}
}

func TestIndexHashDescribesParsedSnapshot(t *testing.T) {
	root := newTinyModule(t)
	path := filepath.Join(root, "a.go")
	ctx := context.Background()
	pkgs, hashes, err := loadGoPackages(ctx, root, ".")
	if err != nil {
		t.Fatal(err)
	}
	parsedHash, err := FileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package tiny\nfunc NewSnapshot() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ix := NewIndexer(store, root)
	if err := ix.indexPackage(ctx, pkgs[0], collectNamedTypes(pkgs), &IndexStats{}, nil, hashes, map[string]bool{}, nil); err != nil {
		t.Fatal(err)
	}
	if hash, err := store.FileHash(ctx, path); err != nil || hash != parsedHash {
		t.Fatalf("stored hash = %s, %v; want parsed snapshot %s", hash, err, parsedHash)
	}
	if _, err := ix.IndexFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	if syms, err := store.SymbolsInFile(ctx, path); err != nil || len(syms) != 1 || syms[0].Name != "NewSnapshot" {
		t.Fatalf("new snapshot was incorrectly skipped: %v, %v", syms, err)
	}
}

func TestIndexAllOwnRepo(t *testing.T) {
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
	stats, err := ix.IndexAll(context.Background())
	if err != nil {
		t.Fatalf("IndexAll: %v", err)
	}
	if stats.Files == 0 {
		t.Fatal("expected at least one indexed file")
	}
	if stats.Symbols == 0 {
		t.Fatal("expected at least one indexed symbol")
	}
	t.Logf("indexed %d files, %d packages, %d symbols, %d edges in %s",
		stats.Files, stats.Packages, stats.Symbols, stats.Edges, stats.Elapsed)

	dbStats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if dbStats.Symbols != stats.Symbols {
		t.Fatalf("store has %d symbols, indexer reported %d", dbStats.Symbols, stats.Symbols)
	}

	syms, err := store.FindSymbols(context.Background(), "New", "func")
	if err != nil {
		t.Fatalf("FindSymbols: %v", err)
	}
	t.Logf("found %d symbols named New (func)", len(syms))
}

// TestIndexAllChronosFramework validates the P1-007 acceptance criterion
// (index the Chronos repo in <15s) when the sibling ../chronos checkout is
// present. It's skipped otherwise so the suite doesn't depend on a specific
// local layout.
func TestIndexAllChronosFramework(t *testing.T) {
	root, err := filepath.Abs("../../../chronos")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("sibling chronos checkout not found at %s: %v", root, err)
	}

	dbPath := filepath.Join(t.TempDir(), "graph.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	ix := NewIndexer(store, root)
	start := time.Now()
	stats, err := ix.IndexAll(context.Background())
	if err != nil {
		t.Fatalf("IndexAll: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("indexed %d files, %d packages, %d symbols, %d edges in %s",
		stats.Files, stats.Packages, stats.Symbols, stats.Edges, elapsed)

	budget := 15 * time.Second
	if raceEnabled {
		// The race detector instruments every memory access, which slows
		// go/packages' type-checking pass well past the un-instrumented
		// budget; relax it rather than asserting a number this test isn't
		// actually measuring.
		budget *= 3
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		// Shared GitHub-hosted runners are noisy neighbors and run well
		// behind a dev machine on CPU-bound work; widen further so this
		// PRD acceptance check doesn't flake on runner speed variance.
		budget *= 2
	}
	if elapsed > budget {
		t.Errorf("indexing took %s, want <%s (PRD P1-007 acceptance)", elapsed, budget)
	}
}
