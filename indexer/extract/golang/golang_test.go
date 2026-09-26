package golang

import (
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

const sample = `package demo

import (
	"fmt"
	str "strings"
	"example.com/mod/v2"
	_ "embed"
)

// Engine runs things.
type Engine struct{ n int }

// Runner is an interface.
type Runner interface{ Run() error }

type ID = string

const Max = 3

var global = helper()

// Run starts the engine.
func (e *Engine) Run() error {
	fmt.Println(str.ToUpper("x"))
	e.step()
	helper()
	mod.Do()
	return nil
}

func (e *Engine) step() {}

func helper() int {
	f := func() { inner() }
	f()
	return Generic[int](1)
}

func Generic[T any](v T) T { return v }
func inner() {}
`

func TestExtract(t *testing.T) {
	f := &facts.File{Path: "demo.go"}
	Extract(f, []byte(sample))
	if f.ParseErr != "" {
		t.Fatalf("parse error: %s", f.ParseErr)
	}
	if f.PkgName != "demo" {
		t.Fatalf("pkg = %q", f.PkgName)
	}
	syms := map[string]facts.Symbol{}
	for _, s := range f.Symbols {
		syms[s.Qualified()] = s
	}
	for name, kind := range map[string]string{
		"Engine": facts.KindStruct, "Runner": facts.KindInterface, "ID": facts.KindType,
		"Max": facts.KindConst, "global": facts.KindVar, "Engine.Run": facts.KindMethod,
		"Engine.step": facts.KindMethod, "helper": facts.KindFunc, "Generic": facts.KindFunc,
	} {
		s, ok := syms[name]
		if !ok || s.Kind != kind {
			t.Errorf("symbol %s = %+v, want kind %s", name, s, kind)
		}
	}
	run := syms["Engine.Run"]
	if run.Signature != "func (e *Engine) Run() error" || run.Receiver != "*Engine" || run.Doc != "Run starts the engine." {
		t.Errorf("Run = %+v", run)
	}
	if run.Line != 23 || run.EndLine != 29 {
		t.Errorf("Run lines = %d-%d", run.Line, run.EndLine)
	}
	if syms["ID"].Signature != "type ID = string" {
		t.Errorf("ID signature = %q", syms["ID"].Signature)
	}

	type key struct{ caller, callee, qual string }
	got := map[key]uint8{}
	for _, c := range f.Refs {
		if c.Kind != facts.RefCall {
			t.Errorf("Go extractor emitted ref kind %d", c.Kind)
		}
		caller := "<init>"
		if c.Enclosing != facts.NoCaller {
			caller = f.Symbols[c.Enclosing].Qualified()
		}
		got[key{caller, c.Name, c.Qualifier}] = c.QualKind
	}
	want := map[key]uint8{
		{"Engine.Run", "Println", "fmt"}:           facts.QualPackage,
		{"Engine.Run", "ToUpper", "strings"}:       facts.QualPackage,
		{"Engine.Run", "step", "e"}:                facts.QualExpr,
		{"Engine.Run", "helper", ""}:               facts.QualNone,
		{"Engine.Run", "Do", "example.com/mod/v2"}: facts.QualPackage,
		{"helper", "inner", ""}:                    facts.QualNone,
		{"helper", "f", ""}:                        facts.QualNone,
		{"helper", "Generic", ""}:                  facts.QualNone,
		{"global", "helper", ""}:                   facts.QualNone,
	}
	for k, kind := range want {
		if g, ok := got[k]; !ok || g != kind {
			t.Errorf("call %+v: got kind %d present=%v, want %d", k, g, ok, kind)
		}
	}
	if len(f.Imports) != 4 {
		t.Errorf("imports = %+v", f.Imports)
	}
}

func TestExtractSyntaxErrorKeepsPartialFacts(t *testing.T) {
	f := &facts.File{Path: "bad.go"}
	Extract(f, []byte("package bad\n\nfunc Good() {}\n\nfunc Broken( {\n"))
	if f.ParseErr == "" {
		t.Fatal("expected parse error")
	}
	if len(f.Symbols) == 0 || f.Symbols[0].Name != "Good" {
		t.Fatalf("symbols = %+v", f.Symbols)
	}
}

func TestDefaultImportName(t *testing.T) {
	for path, want := range map[string]string{
		"fmt": "fmt", "example.com/mod/v2": "mod", "gopkg.in/yaml.v3": "yaml", "a/go-foo": "go_foo",
	} {
		if got := defaultImportName(path); got != want {
			t.Errorf("%s: got %s want %s", path, got, want)
		}
	}
}

func TestInterfaceMembersAndEmbeds(t *testing.T) {
	src := `package demo

import "io"

type ReadSaver interface {
	io.Reader
	Saver
	// Save persists v.
	Save(v any) (int, error)
	Flush()
	~int | string
}

type Saver interface{ Save(v any) (int, error) }

type Box struct {
	*Base
	io.Writer
	List[int]
	name string
}
`
	f := &facts.File{Path: "demo.go"}
	Extract(f, []byte(src))
	if f.ParseErr != "" {
		t.Fatal(f.ParseErr)
	}
	type rec struct{ name, kind, recv, sig, parent string }
	var got []rec
	for _, s := range f.Symbols {
		parent := ""
		if p, ok := s.Parent(); ok {
			parent = f.Symbols[p].Name
		}
		got = append(got, rec{s.Name, s.Kind, s.Receiver, s.Signature, parent})
	}
	want := []rec{
		{"ReadSaver", facts.KindInterface, "", "type ReadSaver interface", ""},
		{"Reader", facts.KindEmbed, "", "io.Reader", "ReadSaver"},
		{"Saver", facts.KindEmbed, "", "Saver", "ReadSaver"},
		{"Save", facts.KindMethod, "ReadSaver", "Save(v any) (int, error)", "ReadSaver"},
		{"Flush", facts.KindMethod, "ReadSaver", "Flush()", "ReadSaver"},
		{"Saver", facts.KindInterface, "", "type Saver interface", ""},
		{"Save", facts.KindMethod, "Saver", "Save(v any) (int, error)", "Saver"},
		{"Box", facts.KindStruct, "", "type Box struct", ""},
		{"Base", facts.KindEmbed, "", "*Base", "Box"},
		{"Writer", facts.KindEmbed, "", "io.Writer", "Box"},
		{"List", facts.KindEmbed, "", "List[int]", "Box"},
	}
	if len(got) != len(want) {
		t.Fatalf("symbols:\n got %+v\nwant %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("symbol %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	if f.Symbols[3].Doc != "Save persists v." {
		t.Fatalf("method spec doc = %q", f.Symbols[3].Doc)
	}
}

func TestExtractV2Fields(t *testing.T) {
	src := `// Code generated by stringer; DO NOT EDIT.

package demo

import . "strings"

type Saver interface{ Save() }

// Old does things.
//
// Deprecated: use New.
func Old() {}

func TestOld(t *testing.T) { Old() }

func hidden()

func (s *S) run() {}
`
	f := &facts.File{Path: "demo_test.go"}
	Extract(f, []byte(src))
	if f.ParseErr != "" {
		t.Fatal(f.ParseErr)
	}
	if !f.Test || !f.Generated {
		t.Errorf("test=%v generated=%v, want both", f.Test, f.Generated)
	}
	if len(f.Imports) != 1 || f.Imports[0].Kind != facts.ImportWildcard {
		t.Errorf("dot import = %+v", f.Imports)
	}
	syms := map[string]facts.Symbol{}
	for _, s := range f.Symbols {
		syms[s.Qualified()] = s
	}
	for name, want := range map[string]struct {
		vis  uint8
		mods uint32
	}{
		"Saver":      {facts.VisPublic, 0},
		"Saver.Save": {facts.VisPublic, facts.ModDecl},
		"Old":        {facts.VisPublic, facts.ModDeprecated},
		"TestOld":    {facts.VisPublic, facts.ModTest},
		"hidden":     {facts.VisPackage, facts.ModDecl},
		"S.run":      {facts.VisPackage, 0},
	} {
		s, ok := syms[name]
		if !ok || s.Visibility != want.vis || s.Modifiers != want.mods {
			t.Errorf("%s: vis=%d mods=%b, want vis=%d mods=%b (found %v)", name, s.Visibility, s.Modifiers, want.vis, want.mods, ok)
		}
	}

	plain := &facts.File{Path: "demo.go"}
	Extract(plain, []byte("package demo\n\n// Code generated elsewhere, not a header.\nfunc TestX() {}\n"))
	if plain.Test || plain.Generated || plain.Symbols[0].Modifiers&facts.ModTest != 0 {
		t.Errorf("non-test, non-generated file flagged: %+v", plain)
	}
}

func TestTypeRefsAndBindingHints(t *testing.T) {
	const src = `package shop

import "example.com/shop/store"

type Service struct {
	repo  store.Repo
	cache *Cache
	items []Item
}

func (s *Service) Handle(o Order, n int) (*Receipt, error) {
	c := &Cache{}
	r := store.NewRepo()
	var w Writer
	b, err := build()
	x := v.(Reader)
	p := new(Point)
	_ = Item{}
	return nil, nil
}

func Map[K comparable, V any](m map[K]V) []V { return nil }
`
	f := &facts.File{Path: "shop.go"}
	Extract(f, []byte(src))
	if f.ParseErr != "" {
		t.Fatal(f.ParseErr)
	}
	hints := map[string]string{}
	for _, h := range f.Hints {
		scope := "-"
		if h.Scope >= 0 {
			scope = f.Symbols[h.Scope].Name
		}
		hints[scope+":"+h.Name] = h.Type
	}
	for key, want := range map[string]string{
		"Service:repo": "store.Repo", "Service:cache": "Cache",
		"Handle:s": "Service", "Handle:o": "Order",
		"Handle:c": "Cache", "Handle:r": "store.NewRepo()", "Handle:w": "Writer",
		"Handle:b": "build()", "Handle:x": "Reader", "Handle:p": "Point",
	} {
		if hints[key] != want {
			t.Errorf("hint %s = %q, want %q (all: %v)", key, hints[key], want, hints)
		}
	}
	for _, key := range []string{"Service:items", "Handle:n", "Handle:err"} {
		if _, ok := hints[key]; ok {
			t.Errorf("unexpected hint %s", key)
		}
	}
	types := map[string]uint8{}
	for _, r := range f.Refs {
		if r.Kind != facts.RefCall {
			types[r.Name+"/"+r.Qualifier] = r.Kind
		}
	}
	for key, kind := range map[string]uint8{
		"Repo/example.com/shop/store": facts.RefTypeUse, "Cache/": facts.RefInstantiate, "Item/": facts.RefInstantiate,
		"Order/": facts.RefTypeUse, "Receipt/": facts.RefTypeUse, "Writer/": facts.RefTypeUse, "Reader/": facts.RefTypeUse,
	} {
		if got, ok := types[key]; !ok || got != kind {
			t.Errorf("ref %s = %d (present %v), want kind %d; all %v", key, got, ok, kind, types)
		}
	}
	for _, key := range []string{"int/", "error/", "K/", "V/", "comparable/", "o/", "s/"} {
		if _, ok := types[key]; ok {
			t.Errorf("unexpected type ref %s", key)
		}
	}
}
