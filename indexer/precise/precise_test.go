package precise

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

var shopFiles = map[string]string{
	"go.mod": "module example.com/p\n\ngo 1.22\n",
	"shop/shop.go": `package shop

import (
	"fmt"
	"os"
	"strings"
)

type Repo interface{ Save(o Order) error }

type Order struct{ ID string }

type memRepo struct{}

func (m *memRepo) Save(o Order) error { return nil }

// fakeRepo has a Save of another signature: it does not implement Repo.
type fakeRepo struct{}

func (fakeRepo) Save(id string) error { return nil }

type Base struct{}

func (Base) Close() error { return nil }

type Wrapped struct{ Base }

func Handle(r Repo, w Wrapped) error {
	f, _ := os.Open("x")
	f.Close()
	w.Close()
	fmt.Println(strings.ToUpper("a"))
	fn := func() {}
	fn()
	_ = len("x")
	_ = Order(Order{})
	return r.Save(Order{})
}

func Generic[T any](t T) T { return t }

func UseGeneric() { Generic[int](1); Generic("s") }
`,
	"shop/shop_test.go": "package shop\n\nimport \"testing\"\n\nfunc TestHandle(t *testing.T) { _ = Handle(nil, Wrapped{}) }\n",
	"shop/ext_test.go":  "package shop_test\n\nimport (\n\t\"testing\"\n\n\t\"example.com/p/shop\"\n)\n\nfunc TestExt(t *testing.T) { _ = shop.Handle(nil, shop.Wrapped{}) }\n",
	"other/other.go":    "package other\n\nimport \"example.com/p/shop\"\n\nfunc Use() { _ = shop.Handle(nil, shop.Wrapped{}) }\n",
	"broken/broken.go":  "package broken\n\nfunc F() int { return \"not an int\" }\n",
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func loadFixture(t *testing.T, files map[string]string, dirs []string) (string, *Result) {
	t.Helper()
	if err := Available(); err != nil {
		t.Skip(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, files)
	res, err := Load(context.Background(), root, ".", dirs, []string{"GOFLAGS=-mod=mod", "GOTOOLCHAIN=local"})
	if err != nil {
		t.Fatal(err)
	}
	return root, res
}

func dirOf(t *testing.T, res *Result, path string) *Dir {
	t.Helper()
	for _, d := range res.Dirs {
		if d.Path == path {
			return d
		}
	}
	t.Fatalf("no facts for %s; failed %v", path, res.Failed)
	return nil
}

// callsNamed returns the calls of name in file, in order.
func callsNamed(d *Dir, file, name string) []Call {
	var out []Call
	for _, c := range d.Files[file].Calls {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

func TestLoadCallTargets(t *testing.T) {
	_, res := loadFixture(t, shopFiles, nil)
	shop := dirOf(t, res, "shop")
	if shop.Pkg != "example.com/p/shop" {
		t.Errorf("Pkg = %q", shop.Pkg)
	}
	for _, f := range []string{"shop/shop.go", "shop/shop_test.go", "shop/ext_test.go"} {
		if fc, ok := shop.Files[f]; !ok || fc.Hash == 0 {
			t.Errorf("no facts or hash for %s", f)
		}
	}
	check := func(name string, want Call) {
		t.Helper()
		got := callsNamed(shop, "shop/shop.go", name)
		if len(got) == 0 {
			t.Fatalf("no call of %s", name)
		}
		g := got[0]
		if g.File != want.File || g.Recv != want.Recv || g.Name != want.Name {
			t.Errorf("%s -> %+v, want %+v", name, g, want)
		}
	}
	check("Save", Call{File: "shop/shop.go", Name: "Save", Recv: "Repo"}) // interface method
	check("Close", Call{Name: "Close"})                                   // (*os.File).Close: outside the workspace
	if got := callsNamed(shop, "shop/shop.go", "Close"); len(got) != 2 || got[1].File != "shop/shop.go" || got[1].Recv != "Base" {
		t.Errorf("promoted Close = %+v", got)
	}
	check("Println", Call{Name: "Println"})
	check("fn", Call{Name: "fn"})   // a function value
	check("len", Call{Name: "len"}) // a builtin
	check("Order", Call{File: "shop/shop.go", Name: "Order"})
	if got := callsNamed(shop, "shop/shop.go", "Generic"); len(got) != 2 || got[0].File != "shop/shop.go" || got[1].File != "shop/shop.go" {
		t.Errorf("generic calls = %+v", got)
	}
	// Call sites are keyed by the opening parenthesis, as the extractor records them.
	if c := callsNamed(shop, "shop/shop.go", "Save")[0]; c.Line != 37 || c.Col != 15 {
		t.Errorf("Save call at %d:%d", c.Line, c.Col)
	}
	// Tests and external test packages, and other packages.
	if got := callsNamed(shop, "shop/ext_test.go", "Handle"); len(got) != 1 || got[0].File != "shop/shop.go" {
		t.Errorf("external test call = %+v", got)
	}
	if got := callsNamed(dirOf(t, res, "other"), "other/other.go", "Handle"); len(got) != 1 || got[0].File != "shop/shop.go" {
		t.Errorf("cross-package call = %+v", got)
	}
	// Identifiers naming types: Order{} in Handle, and the embedded Base.
	var orders, bases int
	for _, c := range shop.Files["shop/shop.go"].TypeRefs {
		switch {
		case c.Name == "Order" && c.File == "shop/shop.go":
			orders++
		case c.Name == "Base" && c.File == "shop/shop.go":
			bases++
		case c.Name == "T" || c.Name == "any":
			t.Errorf("type parameter or predeclared type recorded: %+v", c)
		}
	}
	if orders < 3 || bases != 2 { // Base: the embedded field and a receiver
		t.Errorf("type refs: %d Order, %d Base", orders, bases)
	}
	// A type error leaves the directory out.
	if _, ok := res.Failed["broken"]; !ok {
		t.Errorf("broken package not reported: %v", res.Failed)
	}
}

func TestLoadMethodSets(t *testing.T) {
	_, res := loadFixture(t, shopFiles, []string{"shop"})
	shop := dirOf(t, res, "shop")
	types := map[string]Type{}
	for _, ty := range shop.Types {
		types[ty.Name] = ty
	}
	repo := types["Repo"]
	if !repo.Iface || len(repo.Methods) != 1 {
		t.Fatalf("Repo = %+v", repo)
	}
	if !types["memRepo"].Implements(repo) {
		t.Errorf("memRepo does not implement Repo: %+v", types["memRepo"])
	}
	if types["fakeRepo"].Implements(repo) {
		t.Errorf("fakeRepo (Save(string)) implements Repo")
	}
	if _, ok := types["Generic"]; ok {
		t.Errorf("functions are not types")
	}
	if got := types["Wrapped"].Methods; len(got) != 1 || got[0] != "Close()(error)" {
		t.Errorf("Wrapped method set = %q", got)
	}
	if len(shop.API) != 1 || shop.API["shop/shop.go"] == 0 {
		t.Errorf("API files = %v", shop.API)
	}
	if len(res.Dirs) != 1 {
		t.Errorf("loaded %d dirs for one pattern", len(res.Dirs))
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := &Dir{Path: "a/b", Pkg: "x/a/b", Files: map[string]File{"a/b/c.go": {Hash: 7, Calls: []Call{{Line: 1, Col: 2, Name: "f"}}}},
		Types: []Type{{Name: "T", Methods: []string{"M()()"}}}, API: map[string]uint64{"a/b/c.go": 7}}
	v := s.Version()
	if err := s.Put([]*Dir{d}); err != nil {
		t.Fatal(err)
	}
	if s.Version() == v {
		t.Error("version not bumped")
	}
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Get("a/b")
	if got == nil || got.Files["a/b/c.go"].Calls[0].Name != "f" || got.Types[0].Name != "T" {
		t.Fatalf("round trip = %+v", got)
	}
	if s2.Get("nope") != nil {
		t.Error("absent dir returned facts")
	}
	if all := s2.All(); len(all) != 1 {
		t.Errorf("All = %d dirs", len(all))
	}
	s2.Delete([]string{"a/b"})
	if s2.Get("a/b") != nil {
		t.Error("deleted dir returned facts")
	}
	// Another version empties the store.
	if err := os.WriteFile(filepath.Join(dir, versionFile), []byte("precise-0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Put([]*Dir{d}); err != nil {
		t.Fatal(err)
	}
	s3, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s3.Get("a/b") != nil {
		t.Error("store of another version kept")
	}
}
