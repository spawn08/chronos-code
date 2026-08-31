package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
	if elapsed > budget {
		t.Errorf("indexing took %s, want <%s (PRD P1-007 acceptance)", elapsed, budget)
	}
}
