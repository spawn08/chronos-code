package retrieve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos-code/indexer"
	"github.com/spawn08/chronos-code/indexer/query"
)

var files = map[string]string{
	"go.mod": "module example.com/shop\n\ngo 1.24\n",
	"store/store.go": `package store

type Order struct{ ID string }

type Repo struct{}

// Save persists an order.
func (r *Repo) Save(o Order) error { return validate(o) }

func validate(o Order) error { return nil }

// NewRepo returns a Repo.
func NewRepo() *Repo { return &Repo{} }
`,
	"api/api.go": `package api

import "example.com/shop/store"

func Handle() error {
	r := store.NewRepo()
	return r.Save(store.Order{})
}
`,
	"api/api_test.go": "package api\n\nimport \"testing\"\n\nfunc TestHandle(t *testing.T) { _ = Handle() }\n",
	// other does not import store: its x.Save() must not resolve to Repo.Save.
	"other/other.go": "package other\n\ntype saver interface{ Save() }\n\nfunc Flush(x saver) { x.Save() }\n",
}

type env struct {
	root string
	eng  *indexer.Engine
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	for name, text := range files {
		writeFile(t, root, name, text)
	}
	eng, err := indexer.Open(indexer.Options{Root: root, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	if _, err := eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &env{root: root, eng: eng}
}

func writeFile(t *testing.T, root, name, text string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *env) view(t *testing.T) *query.View {
	t.Helper()
	sn := e.eng.Snapshot()
	t.Cleanup(sn.Release)
	return query.NewView(sn, nil)
}

func find(res Result, name string) *Item {
	for i := range res.Items {
		if res.Items[i].Symbol.Name == name {
			return &res.Items[i]
		}
	}
	return nil
}

func TestRetrieveExpandsCallGraphWithReasons(t *testing.T) {
	v := newEnv(t).view(t)
	res := Retrieve(v, Request{Query: "fix validation in validate", Callers: true, Tests: true, Callees: true, Excerpts: true, Budget: 4096})
	if len(res.Items) == 0 || res.Items[0].Symbol.Name != "validate" || res.Items[0].Role != "definition" || res.Items[0].Zoom != Excerpt {
		t.Fatalf("seed first: %+v", res.Items)
	}
	save := find(res, "Save")
	if save == nil || save.Role != "caller" || save.Why != "calls validate" || save.Target != "validate" {
		t.Fatalf("direct caller: %+v", save)
	}
	handle := find(res, "Handle")
	if handle == nil || handle.Role != "caller" || handle.Target != "Repo.Save" {
		t.Fatalf("caller of caller (method call, name matched): %+v", handle)
	}
	if test := find(res, "TestHandle"); test == nil || test.Role != "test" {
		t.Fatalf("test reaching the seed: %+v", test)
	}
	if find(res, "Flush") != nil {
		t.Fatalf("x.Save() in a package that does not import store must not link: %+v", res.Items)
	}
	noTests := Retrieve(v, Request{Query: "validate", Callers: true, Excerpts: true, Budget: 4096})
	if find(noTests, "TestHandle") != nil {
		t.Fatal("tests included without Tests")
	}
	noCallers := Retrieve(v, Request{Query: "validate", Excerpts: true, Budget: 4096})
	if find(noCallers, "Save") != nil {
		t.Fatal("callers included without Callers")
	}
}

func TestRetrieveMissesAndSeeds(t *testing.T) {
	v := newEnv(t).view(t)
	res := Retrieve(v, Request{Query: "why does FooBarMissing break the store/store.go repo", Callers: true, Budget: 4096})
	if len(res.Misses) != 1 || res.Misses[0] != "FooBarMissing" {
		t.Fatalf("misses = %v", res.Misses)
	}
	if find(res, "NewRepo") == nil || find(res, "Save") == nil {
		t.Fatalf("path seed must bring the file's declarations: %+v", res.Items)
	}
	res = Retrieve(v, Request{Symbols: []string{"NewRepo", "Nope"}, Budget: 4096})
	if len(res.Misses) != 1 || res.Misses[0] != "Nope" || len(res.Items) == 0 || res.Items[0].Symbol.Name != "NewRepo" {
		t.Fatalf("explicit symbols: %+v", res)
	}
	if res := Retrieve(v, Request{Query: "hello there", Budget: 4096}); res.Seeds != 0 || len(res.Items) != 0 {
		t.Fatalf("chat text must not produce context: %+v", res)
	}
}

func TestRetrievePacksWithinBudget(t *testing.T) {
	v := newEnv(t).view(t)
	full := Retrieve(v, Request{Query: "validate", Callers: true, Tests: true, Callees: true, Excerpts: true, Budget: 4096})
	tight := Retrieve(v, Request{Query: "validate", Callers: true, Tests: true, Callees: true, Excerpts: true, Budget: 60, ItemOverhead: 30})
	if len(tight.Items) == 0 || len(tight.Items) >= len(full.Items) || tight.Omitted["budget"] == 0 {
		t.Fatalf("tight budget: %d items, omitted %v (full %d)", len(tight.Items), tight.Omitted, len(full.Items))
	}
	for _, it := range tight.Items {
		if it.Zoom == Excerpt {
			t.Fatalf("no excerpt fits 60 tokens: %+v", it)
		}
	}
}

func TestRetrieveSubtractsSeenAndReportsChanges(t *testing.T) {
	e := newEnv(t)
	seen := NewSeen()
	req := Request{Query: "validate", Callers: true, Excerpts: true, Budget: 4096, Seen: seen}
	first := Retrieve(e.view(t), req)
	it := find(first, "validate")
	if it == nil || it.Zoom != Excerpt {
		t.Fatalf("first: %+v", first.Items)
	}
	meta, _ := e.view(t).FileMeta(it.Symbol.File)
	seen.Add(it.Symbol.File, meta.Hash, it.Start, it.End)
	second := Retrieve(e.view(t), req)
	if again := find(second, "validate"); again == nil || again.Zoom != Signature || second.Omitted["already_seen"] == 0 {
		t.Fatalf("seen source repeated: %+v %v", again, second.Omitted)
	}
	writeFile(t, e.root, "store/store.go", files["store/store.go"]+"\nfunc Extra() {}\n")
	if _, err := e.eng.Update(context.Background(), []string{"store/store.go"}); err != nil {
		t.Fatal(err)
	}
	third := Retrieve(e.view(t), req)
	if len(third.Invalidated) != 1 || third.Invalidated[0] != "store/store.go" {
		t.Fatalf("invalidated = %v", third.Invalidated)
	}
	if again := find(third, "validate"); again == nil || again.Zoom != Excerpt {
		t.Fatalf("changed file must be delivered again: %+v", again)
	}
}

func TestSeenRanges(t *testing.T) {
	s := NewSeen()
	s.Add("a.go", 1, 10, 20)
	s.Add("a.go", 1, 18, 30)
	if !s.Covered("a.go", 1, 12, 30) || s.Covered("a.go", 1, 5, 12) || s.Covered("a.go", 2, 12, 20) || s.Covered("b.go", 1, 1, 1) {
		t.Fatal("coverage")
	}
	if s.Changed("a.go", 1) || !s.Changed("a.go", 2) || s.Changed("a.go", 2) {
		t.Fatal("a change is reported once")
	}
	s.Add("a.go", 0, 1, 2) // unindexed content is never recorded
	if s.Covered("a.go", 0, 1, 2) {
		t.Fatal("zero hash recorded")
	}
}
