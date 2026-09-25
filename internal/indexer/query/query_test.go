package query_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer"
	"github.com/spawn08/chronos-code/internal/indexer/query"
)

var shopFiles = map[string]string{
	"go.mod": "module example.com/shop\n\ngo 1.24\n",
	"store/store.go": `package store

import "io"

// Repo stores orders.
type Repo interface {
	io.Closer
	Save(o Order) error
	Load(id string) (Order, error)
}

type Order struct{ ID string }

type base struct{}

func (base) Close() error { return nil }

type memRepo struct{ base }

func (m *memRepo) Save(o Order) error { return m.helper() }

func (m *memRepo) Load(id string) (Order, error) { return Order{ID: id}, nil }

func (m *memRepo) helper() error { n := len("x"); _ = n; return nil }

// NewRepo returns an in-memory Repo.
func NewRepo() Repo { return &memRepo{} }

type diskRepo struct{}

func (diskRepo) Save(o Order) error { return nil }
`,
	"api/api.go": `package api

import "example.com/shop/store"

func Handle() error {
	r := store.NewRepo()
	return r.Save(store.Order{})
}

func parseHTTPRequest() {}

func Recurse(n int) {
	if n > 0 {
		Recurse(n - 1)
	}
}
`,
	"api/api_test.go": `package api

import "testing"

func TestHandle(t *testing.T) { _ = Handle() }
`,
}

type fixture struct {
	root  string
	eng   *indexer.Engine
	cache *query.Cache
}

func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	root := t.TempDir()
	for name, text := range files {
		write(t, root, name, text)
	}
	eng, err := indexer.Open(indexer.Options{Root: root, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	if _, err := eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &fixture{root: root, eng: eng, cache: query.NewCache()}
}

func write(t *testing.T, root, name, text string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) view(t *testing.T) *query.View {
	t.Helper()
	sn := f.eng.Snapshot()
	t.Cleanup(sn.Release)
	return query.NewView(sn, f.cache)
}

func qualified(syms []query.Symbol) []string {
	out := make([]string, len(syms))
	for i, s := range syms {
		out[i] = s.Qualified()
	}
	slices.Sort(out)
	return out
}

func TestSymbolsExactQualifiedAndEmbedsHidden(t *testing.T) {
	v := newFixture(t, shopFiles).view(t)
	if got, want := qualified(v.Symbols("Save", "")), []string{"Repo.Save", "diskRepo.Save", "memRepo.Save"}; !slices.Equal(got, want) {
		t.Fatalf("Save = %v, want %v", got, want)
	}
	if got := v.Symbols("Save", "method"); len(got) != 3 {
		t.Fatalf("kind filter: %d symbols", len(got))
	}
	spec := v.Symbols("Repo.Save", "")
	if len(spec) != 1 || spec[0].Parent != "Repo" || spec[0].Signature != "Save(o Order) error" {
		t.Fatalf("interface method spec: %+v", spec)
	}
	if got := v.Symbols("store.NewRepo", ""); len(got) != 1 || got[0].Package != "example.com/shop/store" || got[0].File != "store/store.go" {
		t.Fatalf("package-qualified lookup: %+v", got)
	}
	if got := v.Symbols("Closer", ""); len(got) != 0 {
		t.Fatalf("embeds must not be returned as declarations: %+v", got)
	}
	if got := v.Symbols("base", ""); len(got) != 1 || got[0].Kind != "struct" {
		t.Fatalf("base = %+v", got)
	}
	ids := map[uint64]bool{}
	for _, s := range v.Symbols("Save", "") {
		if ids[s.ID] {
			t.Fatalf("duplicate symbol ID %d", s.ID)
		}
		ids[s.ID] = true
	}
}

func TestCallersCalleesAndRecursion(t *testing.T) {
	v := newFixture(t, shopFiles).view(t)
	callers := v.Callers([]string{"Save", "helper", "Recurse", "Missing"})
	if !slices.Equal(callers["Save"], []string{"Handle"}) || !slices.Equal(callers["helper"], []string{"memRepo.Save"}) {
		t.Fatalf("callers = %v", callers)
	}
	if _, ok := callers["Recurse"]; ok {
		t.Fatalf("recursive self-call reported as caller: %v", callers["Recurse"])
	}
	if _, ok := callers["Missing"]; ok {
		t.Fatal("name without callers must be absent")
	}
	if got := v.CallerSymbols("Handle"); len(got) != 1 || got[0].Name != "TestHandle" {
		t.Fatalf("CallerSymbols(Handle) = %+v", got)
	}
	if got := v.Callees("Handle"); !slices.Equal(got, []string{"NewRepo", "Save"}) {
		t.Fatalf("Callees(Handle) = %v", got)
	}
	if got := v.Callees("memRepo.helper"); len(got) != 0 {
		t.Fatalf("builtins must be omitted: %v", got)
	}
}

func TestImplementationsBySyntax(t *testing.T) {
	v := newFixture(t, shopFiles).view(t)
	// memRepo declares Save and Load and gets Close promoted from base;
	// io.Closer's method set is known without indexing the standard library.
	if got := v.Implementations("Repo"); !slices.Equal(got, []string{"memRepo"}) {
		t.Fatalf("Implementations(Repo) = %v", got)
	}
	if got := v.Implementations("Order"); len(got) != 0 {
		t.Fatalf("a struct is not an interface: %v", got)
	}
}

func TestSearchAndFuzzy(t *testing.T) {
	v := newFixture(t, shopFiles).view(t)
	hits := v.Search("parse http request", 5)
	if len(hits) == 0 || hits[0].Name != "parseHTTPRequest" {
		t.Fatalf("camelCase search: %+v", hits)
	}
	hits = v.Search("NewRepo", 5)
	if len(hits) == 0 || hits[0].Name != "NewRepo" {
		t.Fatalf("exact name must rank first: %+v", hits)
	}
	if got := v.Search("in-memory orders", 3); len(got) == 0 {
		t.Fatal("doc words must be searchable")
	}
	if got := v.Search("", 5); got != nil {
		t.Fatalf("empty query: %+v", got)
	}
	names := map[string]bool{}
	for _, s := range v.Fuzzy("repo") {
		names[s.Name] = true
	}
	for _, want := range []string{"NewRepo", "Repo", "diskRepo", "memRepo"} {
		if !names[want] {
			t.Fatalf("Fuzzy(repo) missing %s: %v", want, names)
		}
	}
	if got := v.Fuzzy("NewRepp"); len(got) == 0 || got[0].Name != "NewRepo" {
		t.Fatalf("did-you-mean: %+v", got)
	}
}

func TestPackagesImportsAndStats(t *testing.T) {
	v := newFixture(t, shopFiles).view(t)
	if got := v.Packages(); !slices.Equal(got, []string{"example.com/shop/api", "example.com/shop/store"}) {
		t.Fatalf("Packages = %v", got)
	}
	if got := v.PackageDeps("example.com/shop/api"); !slices.Equal(got, []string{"example.com/shop/store"}) {
		t.Fatalf("PackageDeps = %v", got)
	}
	if got := v.PackageDependents("example.com/shop/store"); !slices.Equal(got, []string{"example.com/shop/api"}) {
		t.Fatalf("PackageDependents = %v", got)
	}
	if got := v.PackageImports("example.com/shop/store"); !slices.Equal(got, []string{"io"}) {
		t.Fatalf("PackageImports = %v", got)
	}
	files := v.PackageFiles("example.com/shop/api")
	if len(files) != 2 || files[0].Path != "api/api.go" || files[1].Path != "api/api_test.go" {
		t.Fatalf("PackageFiles = %+v", files)
	}
	st := v.Stats()
	if st.Files != 3 || st.Packages != 2 || st.Symbols == 0 || st.Calls == 0 {
		t.Fatalf("Stats = %+v", st)
	}
	if got := v.SymbolsInRange("store/store.go", 6, 6); len(got) != 1 || got[0].Name != "Repo" {
		t.Fatalf("SymbolsInRange = %+v", got)
	}
}

// An overlay replaces a file: the replaced records must disappear from every
// query, including the base segment's search postings.
func TestOverlayHidesReplacedRecords(t *testing.T) {
	f := newFixture(t, shopFiles)
	before := f.view(t)
	if len(before.Search("Recurse", 5)) == 0 {
		t.Fatal("fixture must find Recurse")
	}
	write(t, f.root, "api/api.go", "package api\n\nfunc Handle() error { return nil }\n\nfunc Fresh() {}\n")
	if _, err := f.eng.Update(context.Background(), []string{"api/api.go"}); err != nil {
		t.Fatal(err)
	}
	v := f.view(t)
	if got := v.Symbols("Recurse", ""); len(got) != 0 {
		t.Fatalf("replaced symbol still visible: %+v", got)
	}
	for _, h := range v.Search("Recurse", 5) {
		if h.Name == "Recurse" {
			t.Fatalf("replaced symbol still searchable: %+v", h)
		}
	}
	if got := v.Callers([]string{"Save"})["Save"]; len(got) != 0 {
		t.Fatalf("replaced call site still visible: %v", got)
	}
	if got := v.Symbols("Fresh", ""); len(got) != 1 {
		t.Fatalf("new symbol missing: %+v", got)
	}
	if got := v.Fuzzy("fres"); len(got) != 1 || got[0].Name != "Fresh" {
		t.Fatalf("fuzzy over overlay: %+v", got)
	}
}
