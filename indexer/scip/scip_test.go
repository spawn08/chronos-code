package scip_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cespare/xxhash/v2"

	"github.com/spawn08/chronos-code/indexer/precise"
	"github.com/spawn08/chronos-code/indexer/scip"
	"github.com/spawn08/chronos-code/indexer/scip/sciptest"
)

const (
	libTS = `export function greet(name: string): string {
  return "hi " + name;
}

export class Greeter {
  constructor(private who: string) {}
  hello(): string { return greet(this.who); }
}
`
	mainTS = `import { greet, Greeter as G } from "./lib";

const s = "😀"; greet("a");
const g = new G("b");
console.log(g.hello());
`
	pkg        = "scip-typescript npm app 1.0.0 "
	greetSym   = pkg + "`lib.ts`/greet()."
	greeterSym = pkg + "`lib.ts`/Greeter#"
	ctorSym    = pkg + "`lib.ts`/Greeter#`<constructor>`()."
	helloSym   = pkg + "`lib.ts`/Greeter#hello()."
	logSym     = "scip-typescript npm typescript 5.0.0 `lib.dom.d.ts`/Console#log()."
)

func libDoc() sciptest.Document {
	return sciptest.Document{Path: "lib.ts", Language: "typescript", Occurrences: []sciptest.Occurrence{
		sciptest.At(libTS, "greet", 0, greetSym, scip.RoleDefinition),
		sciptest.At(libTS, "Greeter", 0, greeterSym, scip.RoleDefinition),
		sciptest.At(libTS, "constructor", 0, ctorSym, scip.RoleDefinition),
		sciptest.At(libTS, "hello", 0, helloSym, scip.RoleDefinition),
		sciptest.At(libTS, "greet", 1, greetSym, 0),
	}}
}

func mainDoc() sciptest.Document {
	return sciptest.Document{Path: "main.ts", Language: "typescript", Occurrences: []sciptest.Occurrence{
		sciptest.At(mainTS, "greet", 0, greetSym, scip.RoleImport),
		sciptest.At(mainTS, "Greeter", 0, greeterSym, scip.RoleImport),
		sciptest.At(mainTS, "greet", 1, greetSym, 0),
		sciptest.At(mainTS, "G(", 0, ctorSym, 0), // an alias: not the symbol's name
		sciptest.At(mainTS, "hello", 0, helloSym, 0),
		sciptest.At(mainTS, "log", 0, logSym, 0),
	}}
}

type fixture struct {
	root, index string
}

// newFixture writes the workspace, its files dated an hour ago, and then
// the index, so the index is newer than every file.
func newFixture(t *testing.T, docs ...sciptest.Document) *fixture {
	t.Helper()
	root := t.TempDir()
	old := time.Now().Add(-time.Hour)
	for name, src := range map[string]string{"lib.ts": libTS, "main.ts": mainTS} {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if len(docs) == 0 {
		docs = []sciptest.Document{libDoc(), mainDoc()}
	}
	index := filepath.Join(t.TempDir(), "index.scip")
	if err := sciptest.Write(index, "file:///elsewhere/ci", docs...); err != nil {
		t.Fatal(err)
	}
	return &fixture{root: root, index: index}
}

func (f *fixture) indexed(p string) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(p)))
	if err != nil {
		return 0, false
	}
	return xxhash.Sum64(data), true
}

func (f *fixture) run(t *testing.T, srcs ...scip.Source) *scip.Result {
	t.Helper()
	if len(srcs) == 0 {
		srcs = []scip.Source{{Index: f.index}}
	}
	res, err := scip.Import(context.Background(), scip.Options{Root: f.root, Indexed: f.indexed}, srcs)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func file(res *scip.Result, p string) (precise.File, bool) {
	for _, d := range res.Dirs {
		if f, ok := d.Files[p]; ok {
			return f, true
		}
	}
	return precise.File{}, false
}

func TestImportRecordsSitesAndTargets(t *testing.T) {
	f := newFixture(t)
	res := f.run(t)
	if res.Docs != 2 || len(res.Rejected) != 0 {
		t.Fatalf("docs %d, rejected %v", res.Docs, res.Rejected)
	}
	m, ok := file(res, "main.ts")
	if !ok {
		t.Fatal("main.ts not imported")
	}
	if h, _ := f.indexed("main.ts"); m.Hash != h {
		t.Errorf("hash %x, want %x", m.Hash, h)
	}
	// Byte columns, 1-based: the emoji is 4 bytes but 2 UTF-16 units.
	line3 := `const s = "😀"; greet("a");`
	want := []precise.Call{
		{Line: 3, Col: int32(strings.Index(line3, "greet") + 1), File: "lib.ts", Name: "greet", DefLine: 1},
		{Line: 5, Col: int32(strings.Index("console.log(g.hello());", "log") + 1), Name: "log"},
		{Line: 5, Col: int32(strings.Index("console.log(g.hello());", "hello") + 1), File: "lib.ts", Name: "hello", DefLine: 7},
	}
	if !slices.Equal(m.Calls, want) {
		t.Errorf("calls\n got %+v\nwant %+v", m.Calls, want)
	}
	if len(m.TypeRefs) != 0 {
		t.Errorf("type refs %+v: imports and the aliased constructor call are not sites", m.TypeRefs)
	}
	for _, d := range res.Dirs {
		if h, _ := f.indexed("lib.ts"); d.Defs["lib.ts"] != h {
			t.Errorf("target hash %x, want %x", d.Defs["lib.ts"], h)
		}
	}
	l, _ := file(res, "lib.ts")
	if len(l.Calls) != 1 || l.Calls[0].Line != 7 || l.Calls[0].DefLine != 1 {
		t.Errorf("lib.ts calls %+v", l.Calls)
	}
}

func TestImportRejectsStaleDocuments(t *testing.T) {
	// A line inserted above every occurrence, with the file's time kept
	// older than the index: the occurrence check catches it.
	f := newFixture(t)
	p := filepath.Join(f.root, "main.ts")
	st, _ := os.Stat(p)
	if err := os.WriteFile(p, []byte("// header\n"+mainTS), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(p, st.ModTime(), st.ModTime())
	res := f.run(t)
	if _, ok := file(res, "main.ts"); ok || res.Rejected["main.ts"] == "" {
		t.Fatalf("stale main.ts imported (rejected: %v)", res.Rejected)
	}
	if _, ok := file(res, "lib.ts"); !ok {
		t.Error("lib.ts not imported")
	}

	// Any edit after the index was written: the modification time.
	f = newFixture(t)
	p = filepath.Join(f.root, "lib.ts")
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(p, future, future)
	res = f.run(t)
	if !strings.Contains(res.Rejected["lib.ts"], "modified after") {
		t.Fatalf("rejected: %v", res.Rejected)
	}
	// main.ts calls into lib.ts, whose definitions were not imported:
	// those sites are left to the syntactic resolver, the external one
	// stays.
	m, _ := file(res, "main.ts")
	if len(m.Calls) != 1 || m.Calls[0].Name != "log" || m.Calls[0].File != "" {
		t.Errorf("main.ts calls %+v", m.Calls)
	}
}

func TestImportGatesOnIndexedHash(t *testing.T) {
	f := newFixture(t)
	res, err := scip.Import(context.Background(), scip.Options{Root: f.root, Indexed: func(p string) (uint64, bool) {
		h, ok := f.indexed(p)
		if p == "main.ts" {
			h++ // the index has not caught up with the file
		}
		return h, ok
	}}, []scip.Source{{Index: f.index}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected["main.ts"] != "not indexed yet" {
		t.Errorf("rejected: %v", res.Rejected)
	}
}

func TestImportObservedRun(t *testing.T) {
	f := newFixture(t)
	// A watched run: no heuristics, the observed hashes decide, even for
	// a file modified after the index.
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(f.root, "main.ts"), future, future)
	hl, _ := f.indexed("lib.ts")
	res := f.run(t, scip.Source{Index: f.index, Observed: map[string]uint64{"lib.ts": hl, "main.ts": 1}})
	if _, ok := file(res, "lib.ts"); !ok {
		t.Error("lib.ts not imported")
	}
	if !strings.Contains(res.Rejected["main.ts"], "while the indexer ran") {
		t.Errorf("rejected: %v", res.Rejected)
	}
}

func TestImportEmbeddedText(t *testing.T) {
	doc := mainDoc()
	doc.Text, doc.HasText = mainTS+"// changed\n", true
	f := newFixture(t, libDoc(), doc)
	res := f.run(t)
	if !strings.Contains(res.Rejected["main.ts"], "text differs") {
		t.Errorf("rejected: %v", res.Rejected)
	}
}

func TestImportProjectRootAndDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for name, src := range map[string]string{"lib.ts": libTS, "main.ts": mainTS} {
		p := filepath.Join(root, "web", name)
		_ = os.WriteFile(p, []byte(src), 0o644)
		_ = os.Chtimes(p, old, old)
	}
	f := &fixture{root: root}
	// The project root inside the workspace wins over Dir.
	f.index = filepath.Join(t.TempDir(), "a.scip")
	if err := sciptest.Write(f.index, "file://"+filepath.ToSlash(filepath.Join(root, "web")), libDoc(), mainDoc()); err != nil {
		t.Fatal(err)
	}
	if res := f.run(t, scip.Source{Index: f.index, Dir: "elsewhere"}); res.Docs != 2 {
		t.Errorf("project root: %d documents, rejected %v", res.Docs, res.Rejected)
	}
	// A project root outside it (built in CI): Dir.
	f.index = filepath.Join(t.TempDir(), "b.scip")
	if err := sciptest.Write(f.index, "file:///ci/checkout", libDoc(), mainDoc()); err != nil {
		t.Fatal(err)
	}
	if res := f.run(t, scip.Source{Index: f.index, Dir: "web"}); res.Docs != 2 {
		t.Errorf("dir: %d documents, rejected %v", res.Docs, res.Rejected)
	}
	if res := f.run(t, scip.Source{Index: f.index}); res.Docs != 0 || res.Skipped != 2 {
		t.Errorf("no dir: %d documents, %d skipped", res.Docs, res.Skipped)
	}
}

func TestImportSkip(t *testing.T) {
	f := newFixture(t)
	res, err := scip.Import(context.Background(), scip.Options{Root: f.root, Indexed: f.indexed,
		Skip: func(p string) bool { return p == "lib.ts" }}, []scip.Source{{Index: f.index}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Docs != 1 || res.Skipped != 1 {
		t.Errorf("docs %d, skipped %d", res.Docs, res.Skipped)
	}
}

func TestReadStreamsAndReportsTruncation(t *testing.T) {
	data := sciptest.Encode("file:///x", libDoc(), mainDoc())
	var meta scip.Metadata
	var paths []string
	err := scip.Read(bytes.NewReader(data), func(m scip.Metadata) { meta = m }, func(d scip.Document) error {
		paths = append(paths, d.Path)
		return nil
	})
	if err != nil || meta.ProjectRoot != "file:///x" || !slices.Equal(paths, []string{"lib.ts", "main.ts"}) {
		t.Fatalf("err %v, meta %+v, paths %v", err, meta, paths)
	}
	if err := scip.Read(bytes.NewReader(data[:len(data)-3]), nil, func(scip.Document) error { return nil }); err == nil {
		t.Error("truncated index read without error")
	}
}

func TestParseSymbol(t *testing.T) {
	for _, tc := range []struct {
		sym       string
		pkg       string
		last      scip.Descriptor
		spellings []string
		implicit  bool
	}{
		{greetSym, "npm app 1.0.0", scip.Descriptor{Name: "greet", Suffix: scip.SuffixMethod}, []string{"greet"}, false},
		{ctorSym, "npm app 1.0.0", scip.Descriptor{Name: "<constructor>", Suffix: scip.SuffixMethod}, []string{"Greeter", "constructor"}, true},
		{"semanticdb maven jdk 17 java/util/ArrayList#`<init>`(+2).", "maven jdk 17", scip.Descriptor{Name: "<init>", Suffix: scip.SuffixMethod}, []string{"ArrayList", "constructor"}, true},
		{"cxx . . $ tinyxml2/XMLDocument#XMLDocument(a2190919d02f8d13).", ". . $", scip.Descriptor{Name: "XMLDocument", Suffix: scip.SuffixMethod}, []string{"XMLDocument"}, true},
		{"semanticdb maven . . org/KArgumentCaptor#getFirstValue().", "maven . .", scip.Descriptor{Name: "getFirstValue", Suffix: scip.SuffixMethod}, []string{"getFirstValue", "FirstValue", "firstValue", "get"}, false},
		{"semanticdb maven . . kotlin/Function1#invoke().", "maven . .", scip.Descriptor{Name: "invoke", Suffix: scip.SuffixMethod}, []string{"invoke"}, true},
		{"cxx . . $ $anonymous_type_0#", ". . $", scip.Descriptor{Name: "$anonymous_type_0", Suffix: scip.SuffixType}, nil, false},
		{"rust-analyzer cargo walkdir 2.5.0 WalkDir#new().", "cargo walkdir 2.5.0", scip.Descriptor{Name: "new", Suffix: scip.SuffixMethod}, []string{"new"}, false},
		{"scip-python python my  pkg 1.0 `a.b`/f().", "python my pkg 1.0", scip.Descriptor{Name: "f", Suffix: scip.SuffixMethod}, []string{"f"}, false},
	} {
		s, ok := scip.ParseSymbol(tc.sym)
		if !ok {
			t.Errorf("%s: not parsed", tc.sym)
			continue
		}
		if s.Package != tc.pkg || s.Last() != tc.last || !slices.Equal(s.Spellings(), tc.spellings) || s.Implicit() != tc.implicit {
			t.Errorf("%s: package %q last %+v spellings %q implicit %v", tc.sym, s.Package, s.Last(), s.Spellings(), s.Implicit())
		}
	}
	if s, ok := scip.ParseSymbol("local 12"); !ok || !s.Local {
		t.Error("local symbol")
	}
	for _, bad := range []string{"", "scip-go gomod", "scip-go gomod x v1 `unterminated", "a b c d name"} {
		if _, ok := scip.ParseSymbol(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
