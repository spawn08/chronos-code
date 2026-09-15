package graph

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"unicode"
)

func TestReplaceFileRollback(t *testing.T) {
	for _, failure := range []string{"symbol", "edge", "hash"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			old := FileReplacement{Path: "a.go", Package: "p", Mtime: 1, Hash: "old", Symbols: []Symbol{{Name: "Old", Kind: KindFunc}}, Edges: []Edge{{Kind: EdgeCall, FromName: "Old", ToName: "Target"}}}
			if err := store.ReplaceFile(ctx, old); err != nil {
				t.Fatal(err)
			}
			triggers := map[string]string{
				"symbol": `CREATE TRIGGER fail BEFORE INSERT ON symbols WHEN new.name = 'Fail' BEGIN SELECT RAISE(ABORT, 'symbol failure'); END`,
				"edge":   `CREATE TRIGGER fail BEFORE INSERT ON edges WHEN new.to_name = 'Fail' BEGIN SELECT RAISE(ABORT, 'edge failure'); END`,
				"hash":   `CREATE TRIGGER fail BEFORE UPDATE ON files WHEN new.content_hash = 'new' BEGIN SELECT RAISE(ABORT, 'hash failure'); END`,
			}
			if _, err := store.db.Exec(triggers[failure]); err != nil {
				t.Fatal(err)
			}
			next := FileReplacement{Path: "a.go", Package: "q", Mtime: 2, Hash: "new", Symbols: []Symbol{{Name: "New", Kind: KindFunc}, {Name: "Fail", Kind: KindFunc}}, Edges: []Edge{{Kind: EdgeCall, FromName: "New", ToName: "Target"}, {Kind: EdgeCall, FromName: "New", ToName: "Fail"}}}
			if err := store.ReplaceFile(ctx, next); err == nil {
				t.Fatal("expected injected failure")
			}
			var pkg, hash string
			var mtime int64
			if err := store.db.QueryRow(`SELECT package, mtime, content_hash FROM files WHERE path = 'a.go'`).Scan(&pkg, &mtime, &hash); err != nil {
				t.Fatal(err)
			}
			if pkg != "p" || mtime != 1 || hash != "old" {
				t.Fatalf("metadata changed: %s %d %s", pkg, mtime, hash)
			}
			syms, err := store.SymbolsInFile(ctx, "a.go")
			if err != nil || len(syms) != 1 || syms[0].Name != "Old" {
				t.Fatalf("symbols = %v, %v", syms, err)
			}
			callers, err := store.CallersOf(ctx, "Target")
			if err != nil || !reflect.DeepEqual(callers, []string{"Old"}) {
				t.Fatalf("callers = %v, %v", callers, err)
			}
			results, err := store.Search(ctx, "Old", 10)
			if err != nil || len(results) != 1 {
				t.Fatalf("old FTS = %v, %v", results, err)
			}
			results, err = store.Search(ctx, "New", 10)
			if err != nil || len(results) != 0 {
				t.Fatalf("new FTS = %v, %v", results, err)
			}
		})
	}
}

func TestReplaceFileExplicitOwnershipAndStaleEdges(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Legacy mutable state must not affect either concurrent replacement.
	if err := store.ClearFile(ctx, "unrelated.go"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, path := range []string{"a.go", "b.go"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.ReplaceFile(ctx, FileReplacement{Path: path, Package: "p", Hash: "old", Symbols: []Symbol{{Name: "Shared", Kind: KindStruct}}, Edges: []Edge{{Kind: EdgeImplements, FromName: "Shared", ToName: "Interface"}, {Kind: EdgeCall, FromName: "Shared.Method", ToName: "Target"}, {Kind: EdgeCall, FromName: "Shared.Method", ToName: "Target"}}})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := store.ReplaceFile(ctx, FileReplacement{Path: "a.go", Package: "p", Hash: "new"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM edges WHERE source_file != 'b.go'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale/incorrectly owned edges = %d, %v", count, err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM edges WHERE source_file = 'b.go'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("other file edges = %d, %v", count, err)
	}
	if err := store.ReplaceFile(ctx, FileReplacement{Path: "b.go", Symbols: []Symbol{{File: "wrong.go"}}}); err == nil {
		t.Fatal("accepted a mismatched symbol file")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.ReplaceFile(canceled, FileReplacement{Path: "b.go"}); err == nil {
		t.Fatal("accepted canceled replacement")
	}
	if hash, err := store.FileHash(ctx, "b.go"); err != nil || hash != "old" {
		t.Fatalf("hash = %q, %v", hash, err)
	}
}

func TestStoreReopenDoesNotRebuildFTS(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertSymbol(ctx, Symbol{Name: "Persisted", File: "a.go"}); err != nil {
		t.Fatal(err)
	}
	// FTS rebuild deletes its shadow data. Make that operation fail so this
	// proves normal reopen does not silently rebuild an already-current index.
	if _, err := store.db.Exec(`CREATE TRIGGER forbid_fts_rebuild BEFORE DELETE ON symbols_fts_data BEGIN SELECT RAISE(ABORT, 'unexpected FTS rebuild'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	results, err := store.Search(ctx, "Persisted", 10)
	if err != nil || len(results) != 1 {
		t.Fatalf("reopened search = %v, %v", results, err)
	}
}

func TestReplaceFileRemovesLegacyUnownedMethodEdges(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertSymbol(ctx, Symbol{Name: "Run", Kind: KindMethod, File: "a.go", Receiver: "*Worker"}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertEdge(ctx, Edge{Kind: EdgeCall, FromName: "Worker.Run", ToName: "Target"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceFile(ctx, FileReplacement{Path: "a.go", Hash: "new"}); err != nil {
		t.Fatal(err)
	}
	if callers, err := store.CallersOf(ctx, "Target"); err != nil || len(callers) != 0 {
		t.Fatalf("legacy method edge survived: %v, %v", callers, err)
	}
}

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

func TestStoreSearchDottedQuery(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	if err := store.InsertSymbol(ctx, Symbol{
		Name: "Errorf", Kind: KindFunc, Package: "fmt", File: "fmt.go",
		Line: 1, EndLine: 2, Signature: "func Errorf(format string, a ...any) error",
		Doc: "Errorf formats according to a format specifier.",
	}); err != nil {
		t.Fatalf("InsertSymbol: %v", err)
	}

	results, err := store.Search(ctx, "fmt.Errorf", 10)
	if err != nil {
		t.Fatalf("Search(fmt.Errorf): %v", err)
	}
	if len(results) != 1 || results[0].Name != "Errorf" {
		t.Fatalf("Search(fmt.Errorf) = %+v, want Errorf", results)
	}

	results, err = store.Search(ctx, "fmt.go", 10)
	if err != nil {
		t.Fatalf("Search(fmt.go): %v", err)
	}
	if len(results) != 1 || results[0].Name != "Errorf" {
		t.Fatalf("Search(fmt.go) = %+v, want Errorf", results)
	}
}

func TestFindSymbolsFuzzyEscapesLikeWildcards(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	for _, name := range []string{"foobar", "foo_bar", "foo%bar"} {
		if err := store.InsertSymbol(ctx, Symbol{Name: name, Kind: KindFunc, Package: "p", File: name + ".go", Line: 1, EndLine: 2}); err != nil {
			t.Fatalf("InsertSymbol(%s): %v", name, err)
		}
	}

	got, err := store.FindSymbolsFuzzy(ctx, "_bar")
	if err != nil {
		t.Fatalf("FindSymbolsFuzzy(_bar): %v", err)
	}
	if len(got) != 1 || got[0].Name != "foo_bar" {
		t.Fatalf("FindSymbolsFuzzy(_bar) = %+v, want only foo_bar", got)
	}

	got, err = store.FindSymbolsFuzzy(ctx, "%bar")
	if err != nil {
		t.Fatalf("FindSymbolsFuzzy(%%bar): %v", err)
	}
	if len(got) != 1 || got[0].Name != "foo%bar" {
		t.Fatalf("FindSymbolsFuzzy(%%bar) = %+v, want only foo%%bar", got)
	}
}

func TestFTS5MatchQueryQuotesSpecialTokens(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"Router", "Router"},
		{"fmt.Errorf", "fmt Errorf"},
		{"router.go", "router go"},
		{`say "hi"`, "say hi"},
		{"AND", `"AND"`},
		{"or", `"or"`},
		{"NOT", `"NOT"`},
		{"near", `"near"`},
		{"handle shell", "handle shell"},
		{"*Store", "Store"},
		{"Store*", "Store"},
		{"foo*", "foo"},
		{"*foo", "foo"},
		{"C++", "C"},
		{"map[string]any", "map string any"},
		{"github.com/spawn08/chronos-code", "github com spawn08 chronos code"},
		{"encoding/json", "encoding json"},
		{"chan<-int", "chan int"},
		{"name:Router", "name Router"},
		{"{name}:Router", "name Router"},
		{"^Router", "Router"},
		{"NEAR/5", `"NEAR" 5`},
		{"foo AND bar", `foo "AND" bar`},
		{"a||b", "a b"},
		{"a&&b", "a b"},
		{"@file", "file"},
		{"#tag", "tag"},
		{"$var", "var"},
		{"foo=bar", "foo bar"},
		{"100%", "100"},
		{"...", ""},
		{"***", ""},
		{"*", ""},
	}
	for _, tt := range tests {
		if got := fts5MatchQuery(tt.in); got != tt.want {
			t.Errorf("fts5MatchQuery(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStoreSearchAcceptsFTSSyntaxQueries(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	if err := store.InsertSymbol(ctx, Symbol{
		Name: "Router", Kind: KindStruct, Package: "router", File: "router.go",
		Line: 1, EndLine: 2, Doc: "Routes coding tasks.",
	}); err != nil {
		t.Fatalf("InsertSymbol: %v", err)
	}

	queries := []string{
		"fmt.Errorf", "router.go", "encoding/json", "github.com/foo.Bar",
		"*Router", "Router*", "*Router*", "foo*", "*foo",
		"AND", "OR", "NOT", "NEAR", "NEAR/5", "AND OR NOT",
		"name:Router", "{name signature}: Router", "^Router",
		"C++", "operator<<", "chan<-int", "map[string]any",
		"@Router", "#Router", "$Router", "%Router",
		"Router=foo", "a||b", "a&&b", "a|b", "a&b", "foo~bar",
		`say "hi"`, "foo'bar", "`Router`",
		"...", "***", "*", "()", "{}", "[]",
		"foo (bar)", "NOT Router", "Router AND OpenStore",
		"<Router>", "Router?", "Router!", "100%",
		"foo=bar", "col:term", "term^2",
	}
	for _, query := range queries {
		if _, err := store.Search(ctx, query, 10); err != nil {
			t.Errorf("Search(%q): %v", query, err)
		}
	}
	for r := rune(1); r < 127; r++ {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == ' ' {
			continue
		}
		query := "Router" + string(r) + "x"
		if _, err := store.Search(ctx, query, 10); err != nil {
			t.Errorf("Search(%q): %v", query, err)
		}
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
