package query_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/indexer"
	"github.com/spawn08/chronos-code/indexer/precise"
	"github.com/spawn08/chronos-code/indexer/query"
)

var preciseShop = map[string]string{
	"go.mod": "module example.com/shop\n\ngo 1.22\n",
	"store/store.go": `package store

import "os"

type Order struct{ ID string }

// Repo stores orders.
type Repo interface{ Save(o Order) error }

type memRepo struct{}

func (m *memRepo) Save(o Order) error { return nil }

// fileRepo saves by id: same method name, another signature.
type fileRepo struct{}

func (fileRepo) Save(id string) error { return nil }

func (fileRepo) Close() error { return nil }

type logger struct{}

func (logger) Close() error { return nil }

func Handle(r Repo) error {
	f, err := os.Open("orders")
	if err != nil {
		return err
	}
	defer f.Close()
	return r.Save(Order{})
}

func Flush(l logger) error { return l.Close() }
`,
}

type preciseFixture struct {
	root  string
	eng   *indexer.Engine
	cache *query.Cache
}

func newPreciseFixture(t *testing.T, files map[string]string) *preciseFixture {
	t.Helper()
	if err := precise.Available(); err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	for name, text := range files {
		write(t, root, name, text)
	}
	eng, err := indexer.Open(indexer.Options{Root: root, Dir: t.TempDir(), Precise: true, PreciseQuiet: time.Hour,
		PreciseEnv: []string{"GOFLAGS=-mod=mod", "GOTOOLCHAIN=local"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	ctx := context.Background()
	if _, err := eng.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := eng.RunPrecise(ctx); err != nil {
		t.Fatal(err)
	}
	return &preciseFixture{root: root, eng: eng, cache: query.NewCache().SetPrecise(eng.Precise())}
}

func (f *preciseFixture) view(t *testing.T) *query.View {
	t.Helper()
	sn := f.eng.Snapshot()
	t.Cleanup(sn.Release)
	return query.NewView(sn, f.cache)
}

// callsFrom resolves the calls made inside q: "callee -> targets label".
func callsFrom(t *testing.T, v *query.View, q string) map[string]string {
	t.Helper()
	syms := v.ByQualified([]string{q})[q]
	if len(syms) != 1 {
		t.Fatalf("%s: %d declarations", q, len(syms))
	}
	out := map[string]string{}
	for _, c := range v.Outgoing(syms[0]) {
		targets, label := v.Resolve(c)
		var names []string
		for _, s := range targets {
			names = append(names, s.Qualified())
		}
		slices.Sort(names)
		out[c.Callee] = label + " " + joinNames(names)
	}
	return out
}

func joinNames(n []string) string {
	s := "["
	for i, x := range n {
		if i > 0 {
			s += " "
		}
		s += x
	}
	return s + "]"
}

func TestPreciseCallsTypeChecked(t *testing.T) {
	f := newPreciseFixture(t, preciseShop)
	v := f.view(t)
	got := callsFrom(t, v, "Handle")
	// The interface method, not every Save by name; (*os.File).Close is
	// not a workspace declaration, so it resolves to nothing.
	if got["Save"] != "type_checked [Repo.Save]" || got["Close"] != "type_checked []" || got["Open"] != "type_checked []" || got["Order"] != "type_checked [Order]" {
		t.Errorf("Handle calls = %v", got)
	}
	if got := callsFrom(t, v, "Flush"); got["Close"] != "type_checked [logger.Close]" {
		t.Errorf("Flush calls = %v", got)
	}
	// Callers of a method follow type-checked edges.
	edges := v.CallerEdges("logger.Close")
	if len(edges) != 1 || edges[0].Caller.Name != "Flush" || edges[0].Resolution != query.TypeChecked {
		t.Errorf("callers of logger.Close = %+v", edges)
	}
	if edges := v.CallerEdges("fileRepo.Close"); len(edges) != 0 {
		t.Errorf("callers of fileRepo.Close = %+v", edges)
	}

	// Once the file changes, its facts are stale: syntactic answers apply
	// until the next load.
	write(t, f.root, "store/store.go", preciseShop["store/store.go"]+"\nfunc Extra() {}\n")
	if _, err := f.eng.Update(context.Background(), []string{"store/store.go"}); err != nil {
		t.Fatal(err)
	}
	v = f.view(t)
	for callee, res := range callsFrom(t, v, "Handle") {
		if strings.HasPrefix(res, query.TypeChecked) {
			t.Errorf("stale facts used for %s: %s", callee, res)
		}
	}
	if err := f.eng.RunPrecise(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := callsFrom(t, f.view(t), "Handle"); got["Save"] != "type_checked [Repo.Save]" {
		t.Errorf("after reload: %v", got)
	}
}

func TestPreciseImplementationsCompareSignatures(t *testing.T) {
	f := newPreciseFixture(t, preciseShop)
	if got := f.view(t).Implementations("Repo"); !slices.Equal(got, []string{"memRepo"}) {
		t.Errorf("type-checked implementations of Repo = %v", got)
	}
	// Without current method sets, method names match (fileRepo too).
	syntactic := newFixture(t, preciseShop).view(t)
	if got := syntactic.Implementations("Repo"); !slices.Equal(got, []string{"fileRepo", "memRepo"}) {
		t.Errorf("syntactic implementations of Repo = %v", got)
	}
	// Adding a file to the package makes its method sets stale.
	write(t, f.root, "store/extra.go", "package store\n\ntype diskRepo struct{}\n\nfunc (diskRepo) Save(o Order) error { return nil }\n")
	if _, err := f.eng.Update(context.Background(), []string{"store/extra.go"}); err != nil {
		t.Fatal(err)
	}
	if got := f.view(t).Implementations("Repo"); !slices.Contains(got, "diskRepo") {
		t.Errorf("after adding a file = %v", got)
	}
}
