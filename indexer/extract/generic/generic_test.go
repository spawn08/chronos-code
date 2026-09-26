package generic

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer/extract/packs"
	"github.com/spawn08/chronos-code/internal/indexer/extract/treesitter"
	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

var update = flag.Bool("update", false, "rewrite testdata/*/sample.*.golden")

var shared = New(treesitter.New(treesitter.Options{}))

// sample returns the pack's sample file under testdata/<pack>.
func sample(t *testing.T, pk *packs.Pack) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join("testdata", pk.ID, "sample.*"))
	for _, m := range matches {
		if !strings.HasSuffix(m, ".golden") {
			return m
		}
	}
	t.Fatalf("pack %s has no testdata/%s/sample.*", pk.ID, pk.ID)
	return ""
}

func extractFile(t *testing.T, pk *packs.Pack, file string) *facts.File {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	f := &facts.File{Path: filepath.ToSlash(file)}
	shared.Extract(f, pk, src)
	return f
}

// TestPackGoldens extracts every pack's sample and compares a readable
// rendering of the facts with the checked-in golden file.
func TestPackGoldens(t *testing.T) {
	for _, pk := range packs.Default().Packs() {
		t.Run(pk.ID, func(t *testing.T) {
			file := sample(t, pk)
			got := render(extractFile(t, pk, file))
			golden := file + ".golden"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("facts differ from %s (run with -update and review the diff):\n%s", golden, diff(string(want), got))
			}
		})
	}
}

func TestEveryPackHasAQuery(t *testing.T) {
	for _, pk := range packs.Default().Packs() {
		if pk.Query == "" {
			t.Errorf("pack %s has no tags.scm", pk.ID)
			continue
		}
		if err := shared.Check(pk); err != nil {
			t.Errorf("pack %s: %v", pk.ID, err)
		}
	}
}

// TestQueryCaptures rejects capture names the extractor does not know, so a
// typo in a tags.scm cannot silently drop facts.
func TestQueryCaptures(t *testing.T) {
	known := map[string]bool{
		"name": true, "receiver": true, "doc": true, "body": true, "scope": true, "scope.name": true,
		"import": true, "include": true, "import.path": true, "import.alias": true, "import.name": true,
		"import.default": true, "import.wildcard": true, "export": true, "export.name": true,
		"export.alias": true, "export.default": true, "export.all": true, "export.source": true,
		"ref.qualifier": true, "package": true,
		"hint": true, "hint.name": true, "hint.type": true, "hint.call": true,
	}
	for k := range refKinds {
		known[k] = true
	}
	for _, kind := range facts.Kinds {
		known["def."+kind] = kind != facts.KindEmbed
		known["def."+kind+".decl"] = kind != facts.KindEmbed
	}
	for _, pk := range packs.Default().Packs() {
		c := shared.compile(pk)
		if c.err != nil {
			continue // reported by TestEveryPackHasAQuery
		}
		for _, name := range c.q.CaptureNames() {
			if !known[name] && !strings.HasPrefix(name, "_") {
				t.Errorf("pack %s: unknown capture @%s", pk.ID, name)
			}
		}
	}
}

// TestConcurrentExtract runs every pack's sample from many goroutines on a
// fresh extractor, so grammar loading and query compilation race.
func TestConcurrentExtract(t *testing.T) {
	x := New(treesitter.New(treesitter.Options{}))
	var wg sync.WaitGroup
	results := make([][]string, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, pk := range packs.Default().Packs() {
				file := sample(t, pk)
				src, err := os.ReadFile(file)
				if err != nil {
					t.Error(err)
					return
				}
				f := &facts.File{Path: filepath.ToSlash(file)}
				x.Extract(f, pk, src)
				results[i] = append(results[i], render(f))
			}
		}()
	}
	wg.Wait()
	for i := 1; i < len(results); i++ {
		if strings.Join(results[i], "") != strings.Join(results[0], "") {
			t.Fatalf("goroutine %d extracted different facts", i)
		}
	}
}

func render(f *facts.File) string {
	var b strings.Builder
	fmt.Fprintf(&b, "lang: %s\n", f.Lang)
	if f.PkgName != "" {
		fmt.Fprintf(&b, "package: %s\n", f.PkgName)
	}
	if f.ParseErr != "" {
		fmt.Fprintf(&b, "parse error: %s\n", f.ParseErr)
	}
	if f.Test || f.Generated {
		fmt.Fprintf(&b, "flags: test=%v generated=%v\n", f.Test, f.Generated)
	}
	if len(f.Imports) > 0 {
		b.WriteString("imports:\n")
	}
	for _, imp := range f.Imports {
		kind := [...]string{"module", "wildcard", "reexport", "include"}[imp.Kind]
		fmt.Fprintf(&b, "  L%d %s %q", imp.Line, kind, imp.Path)
		if imp.Name != "" {
			fmt.Fprintf(&b, " as %s", imp.Name)
		}
		for _, n := range imp.Names {
			fmt.Fprintf(&b, " {%s", n.Name)
			if n.Alias != "" {
				fmt.Fprintf(&b, " as %s", n.Alias)
			}
			b.WriteString("}")
		}
		b.WriteString("\n")
	}
	if len(f.Exports) > 0 {
		b.WriteString("exports:\n")
	}
	for _, e := range f.Exports {
		fmt.Fprintf(&b, "  L%d %s", e.Line, e.Name)
		if e.SourceName != "" {
			fmt.Fprintf(&b, " = %s", e.SourceName)
		}
		if e.Source != "" {
			fmt.Fprintf(&b, " from %q", e.Source)
		}
		b.WriteString("\n")
	}
	if len(f.Symbols) > 0 {
		b.WriteString("symbols:\n")
	}
	vis := [...]string{"?", "public", "protected", "internal", "private", "package"}
	for i, s := range f.Symbols {
		fmt.Fprintf(&b, "  %d. L%d-%d %s %s %s", i, s.Line, s.EndLine, vis[s.Visibility], s.Kind, s.Qualified())
		if p, ok := s.Parent(); ok {
			fmt.Fprintf(&b, " in=%d", p)
		}
		if m := mods(s.Modifiers); m != "" {
			fmt.Fprintf(&b, " [%s]", m)
		}
		if a := s.Params; a.Known {
			fmt.Fprintf(&b, " params=%d..", a.Min)
			if a.Max != facts.VarArgs {
				fmt.Fprintf(&b, "%d", a.Max)
			}
		}
		fmt.Fprintf(&b, "\n      sig: %s\n", s.Signature)
		if s.Doc != "" {
			fmt.Fprintf(&b, "      doc: %q\n", s.Doc)
		}
	}
	if len(f.Hints) > 0 {
		b.WriteString("hints:\n")
	}
	for _, h := range f.Hints {
		fmt.Fprintf(&b, "  L%d %s: %s", h.Line, h.Name, h.Type)
		if h.Scope >= 0 {
			fmt.Fprintf(&b, " in=%d", h.Scope)
		}
		b.WriteString("\n")
	}
	if len(f.Refs) > 0 {
		b.WriteString("refs:\n")
	}
	kinds := [...]string{"call", "type", "extends", "implements", "instantiate", "decorator"}
	for _, r := range f.Refs {
		fmt.Fprintf(&b, "  L%d:%d %s %s", r.Line, r.Col, kinds[r.Kind], r.Name)
		switch r.QualKind {
		case facts.QualPackage:
			fmt.Fprintf(&b, " pkg=%q", r.Qualifier)
		case facts.QualExpr:
			fmt.Fprintf(&b, " on=%q", r.Qualifier)
		}
		if r.Enclosing >= 0 {
			fmt.Fprintf(&b, " in=%d", r.Enclosing)
		}
		if n, ok := r.NArgs(); ok {
			fmt.Fprintf(&b, " args=%d", n)
		}
		if r.ArgTypes != "" {
			fmt.Fprintf(&b, " types=%s", r.ArgTypes)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func mods(m uint32) string {
	var out []string
	for i, name := range []string{"static", "abstract", "async", "override", "deprecated", "test", "decl", "inactive"} {
		if m&(1<<i) != 0 {
			out = append(out, name)
		}
	}
	return strings.Join(out, ",")
}

// diff shows the first differing lines.
func diff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	shown := 0
	for i := 0; i < max(len(w), len(g)) && shown < 20; i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			fmt.Fprintf(&b, "line %d:\n  want: %s\n  got:  %s\n", i+1, wl, gl)
			shown++
		}
	}
	return b.String()
}

func TestFileFlagsAndErrors(t *testing.T) {
	reg := packs.Default()
	for _, tc := range []struct {
		path, src       string
		test, generated bool
		parseErr        string
	}{
		{path: "pkg/test_store.py", src: "def test_x():\n    pass\n", test: true},
		{path: "tests/helpers.py", src: "x = 1\n", test: true},
		{path: "src/app.test.ts", src: "it('x', () => {})\n", test: true},
		{path: "gen/api_pb2.py", src: "# Generated by the protocol buffer compiler.  DO NOT EDIT!\nx = 1\n", generated: true},
		{path: "lib/broken.py", src: "def f(:\n    pass\n", parseErr: "syntax error at line 1"},
		{path: "lib/ok.rb", src: "def f\nend\n"},
	} {
		f := &facts.File{Path: tc.path}
		shared.Extract(f, reg.ForPath(tc.path), []byte(tc.src))
		if f.Test != tc.test || f.Generated != tc.generated || f.ParseErr != tc.parseErr {
			t.Errorf("%s: test=%v generated=%v err=%q, want %v %v %q",
				tc.path, f.Test, f.Generated, f.ParseErr, tc.test, tc.generated, tc.parseErr)
		}
	}
	// A test function is marked only in a test file.
	f := &facts.File{Path: "pkg/test_store.py"}
	shared.Extract(f, reg.ForPath(f.Path), []byte("def test_x():\n    pass\n"))
	if len(f.Symbols) != 1 || f.Symbols[0].Modifiers&facts.ModTest == 0 {
		t.Errorf("test_x in a test file should be a test: %+v", f.Symbols)
	}
	f = &facts.File{Path: "pkg/store.py"}
	shared.Extract(f, reg.ForPath(f.Path), []byte("def test_x():\n    pass\n"))
	if len(f.Symbols) != 1 || f.Symbols[0].Modifiers&facts.ModTest != 0 {
		t.Errorf("test_x outside a test file is not a test: %+v", f.Symbols)
	}
}

func TestPackWithoutQueryKeepsFileLevelFacts(t *testing.T) {
	pk := &packs.Pack{ID: "x", Language: "x", Grammar: "python", Extensions: []string{".x"}}
	f := &facts.File{Path: "a.x"}
	shared.Extract(f, pk, []byte("def f(): pass\n"))
	if f.Lang != "x" || len(f.Symbols) != 0 || f.ParseErr != "" {
		t.Errorf("file-level only: %+v", f)
	}
}

func TestCleanDoc(t *testing.T) {
	for in, want := range map[string]string{
		"/**\n * Line one.\n * Line two.\n */": "Line one.\nLine two.",
		"/// Rust doc.\n/// More.":             "Rust doc.\nMore.",
		"# A comment.":                         "A comment.",
		`"""Docstring."""`:                     "Docstring.",
		`r'''Raw.'''`:                          "Raw.",
		"-- Lua style":                         "Lua style",
	} {
		if got := cleanDoc(in); got != want {
			t.Errorf("cleanDoc(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitQualified(t *testing.T) {
	for in, want := range map[string][2]string{
		"f": {"f", ""}, "a.b.c": {"c", "a.b"}, "std::vec::Vec": {"Vec", "std::vec"},
		`App\Util\helper`: {"helper", `App\Util`}, "List<User>": {"List", ""},
	} {
		if n, q := splitQualified(in); n != want[0] || q != want[1] {
			t.Errorf("splitQualified(%q) = %q, %q, want %q, %q", in, n, q, want[0], want[1])
		}
	}
}
