package graph

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFilesInPackage(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	for i, file := range []FileRecord{
		{Path: "pkg/z.go", Package: "example/pkg"},
		{Path: "other.go", Package: "example/other"},
		{Path: "pkg/a.go", Package: "example/pkg"},
	} {
		if err := store.UpsertFile(ctx, file.Path, file.Package, int64(i+1)); err != nil {
			t.Fatalf("UpsertFile(%s): %v", file.Path, err)
		}
	}

	got, err := store.FilesInPackage(ctx, "example/pkg")
	if err != nil {
		t.Fatalf("FilesInPackage: %v", err)
	}
	want := []FileRecord{
		{Path: "pkg/a.go", Package: "example/pkg"},
		{Path: "pkg/z.go", Package: "example/pkg"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilesInPackage = %#v, want %#v", got, want)
	}
}

func TestStoreFTSSynchronizesSymbols(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	if err := store.InsertSymbol(ctx, Symbol{Name: "Router", Kind: KindStruct, Package: "router", File: "router.go", Line: 10, EndLine: 20, Doc: "Routes coding tasks."}); err != nil {
		t.Fatalf("InsertSymbol: %v", err)
	}

	assertFTSParity(t, store, 1)

	results, err := store.Search(ctx, "Router", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Name != "Router" || results[0].Rank == 0 {
		t.Fatalf("Search(Router) = %+v, want Router with non-zero rank", results)
	}

	if err := store.RemoveFile(ctx, "router.go"); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	assertFTSParity(t, store, 0)
}

func assertFTSParity(t *testing.T, store *Store, want int) {
	t.Helper()
	var got int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM symbols_fts`).Scan(&got); err != nil {
		t.Fatalf("count symbols_fts: %v", err)
	}
	if got != want {
		t.Fatalf("symbols_fts count = %d, want %d", got, want)
	}
}

func TestStoreSearchMigratesExistingSymbolsAndLimitsResults(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "graph.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE files (path TEXT PRIMARY KEY, package TEXT NOT NULL, mtime INTEGER NOT NULL);
		CREATE TABLE packages (name TEXT PRIMARY KEY, imports TEXT NOT NULL DEFAULT '');
		CREATE TABLE symbols (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, kind TEXT NOT NULL,
			package TEXT NOT NULL, file TEXT NOT NULL, line INTEGER NOT NULL, end_line INTEGER NOT NULL,
			signature TEXT NOT NULL DEFAULT '', doc TEXT NOT NULL DEFAULT '', receiver TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE edges (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, from_name TEXT NOT NULL, to_name TEXT NOT NULL);
		INSERT INTO symbols (name, kind, package, file, line, end_line, doc)
		VALUES ('LegacyRouter', 'struct', 'router', 'legacy.go', 1, 2, 'Routes authentication requests.');
	`)
	if err != nil {
		legacy.Close()
		t.Fatalf("create legacy database: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore migration: %v", err)
	}
	defer store.Close()
	for _, name := range []string{"RouterOne", "RouterTwo"} {
		if err := store.InsertSymbol(ctx, Symbol{Name: name, Kind: KindStruct, Package: "router", File: name + ".go", Line: 1, EndLine: 2, Doc: "Routes requests."}); err != nil {
			t.Fatalf("InsertSymbol(%s): %v", name, err)
		}
	}

	results, err := store.Search(ctx, "router", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search result count = %d, want 1", len(results))
	}
	if results[0].Name != "LegacyRouter" && results[0].Name != "RouterOne" && results[0].Name != "RouterTwo" {
		t.Fatalf("Search result = %+v, want migrated or inserted router symbol", results[0])
	}

	results, err = store.Search(ctx, "router", 0)
	if err != nil {
		t.Fatalf("Search default limit: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("Search default limit result count = %d, want 3", len(results))
	}
	for i := 1; i < len(results); i++ {
		if results[i-1].Rank > results[i].Rank {
			t.Fatalf("results are not BM25-ranked: %+v", results)
		}
	}
}

func BenchmarkSearch(b *testing.B) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(b.TempDir(), "graph.db"))
	if err != nil {
		b.Fatalf("OpenStore: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })
	for i := 0; i < 100_000; i++ {
		doc := "Routes coding tasks."
		if i == 50_000 {
			doc = "Distinctive benchmark symbol."
		}
		if err := store.InsertSymbol(ctx, Symbol{Name: "RouterSymbol", Kind: KindStruct, Package: "router", File: "router.go", Line: i + 1, EndLine: i + 1, Doc: doc}); err != nil {
			b.Fatalf("InsertSymbol: %v", err)
		}
	}
	results, err := store.Search(ctx, "distinctive", 10)
	if err != nil {
		b.Fatalf("verify benchmark search: %v", err)
	}
	if len(results) != 1 {
		b.Fatalf("benchmark search result count = %d, want 1", len(results))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.Search(ctx, "distinctive", 10); err != nil {
			b.Fatalf("Search: %v", err)
		}
	}
}
