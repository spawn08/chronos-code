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
	for _, c := range f.Calls {
		caller := "<init>"
		if c.Caller != facts.NoCaller {
			caller = f.Symbols[c.Caller].Qualified()
		}
		got[key{caller, c.Callee, c.Qualifier}] = c.QualKind
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
