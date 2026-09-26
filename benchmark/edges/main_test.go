package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func occurrence(line, start, end int, symbol string, roles int) []byte {
	var rng []byte
	for _, n := range []int{line, start, end} {
		rng = protowire.AppendVarint(rng, uint64(n))
	}
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, rng)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, symbol)
	if roles != 0 {
		b = protowire.AppendTag(b, 3, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(roles))
	}
	return b
}

func document(path string, occs ...[]byte) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, path)
	b = protowire.AppendTag(b, 6, protowire.VarintType)
	b = protowire.AppendVarint(b, encodingUTF8)
	for _, o := range occs {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendBytes(b, o)
	}
	return b
}

// TestEvaluate indexes a small Go module and scores it against a
// hand-written SCIP index: one call resolves correctly, one resolves to a
// different declaration than SCIP's, and one SCIP reference has no indexer
// reference (coverage).
func TestEvaluate(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module ex\n\ngo 1.24\n",
		"a.go":   "package ex\n\nfunc Helper() {}\n\nfunc Other() {}\n",
		"b.go":   "package ex\n\nfunc Run() {\n\tHelper()\n\tOther()\n\tvar f = Helper\n\t_ = f\n}\n",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const helper, other = "scip-go gomod ex v0 `ex`/Helper().", "scip-go gomod ex v0 `ex`/Other()."
	var idx []byte
	for _, doc := range [][]byte{
		document("a.go", occurrence(2, 5, 11, helper, roleDefinition), occurrence(4, 5, 10, other, roleDefinition)),
		// SCIP says line 5's call targets Helper (a deliberate mismatch), and
		// line 6 references Helper without a call.
		document("b.go", occurrence(3, 1, 7, helper, 0), occurrence(4, 1, 6, helper, 0), occurrence(5, 9, 15, helper, 0)),
	} {
		idx = protowire.AppendTag(idx, 2, protowire.BytesType)
		idx = protowire.AppendBytes(idx, doc)
	}
	scipPath := filepath.Join(t.TempDir(), "index.scip")
	if err := os.WriteFile(scipPath, idx, 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := readSCIP(scipPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := evaluate(root, parsed, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Line 4 resolves correctly; line 5 matches by text but SCIP's answer
	// (Helper) differs from the indexer's (Other); line 6 is a reference the
	// indexer does not record as a call.
	c := res.Calls
	if c.Gold != 3 || c.Matched != 2 || res.Types.Gold != 0 {
		t.Fatalf("calls gold=%d matched=%d, types gold=%d; want 3, 2, 0 (%+v)", c.Gold, c.Matched, res.Types.Gold, res)
	}
	ir := c.Labels["import_resolved"]
	if ir == nil || ir.N != 2 || ir.Top1 != 1 || ir.Any != 1 || c.Recall != 0.5 {
		t.Fatalf("labels = %+v recall %v", c.Labels, c.Recall)
	}
	if c.Coverage != 0.667 {
		t.Fatalf("coverage = %v, want 0.667", c.Coverage)
	}
}

func TestSliceEncodings(t *testing.T) {
	line := "é := f(x)" // é is 2 bytes in UTF-8, 1 unit in UTF-16
	if s, e, text := slice(line, 5, 6, encodingUTF16); text != "f" || s != 6 || e != 7 {
		t.Fatalf("utf16 slice = %d %d %q", s, e, text)
	}
	if _, _, text := slice(line, 6, 7, encodingUTF8); text != "f" {
		t.Fatalf("utf8 slice = %q", text)
	}
	if _, _, text := slice(line, 5, 6, encodingUTF32); text != "f" {
		t.Fatalf("utf32 slice = %q", text)
	}
}

func TestWriteMergesByName(t *testing.T) {
	out := filepath.Join(t.TempDir(), "baseline.json")
	for _, r := range []Result{{Name: "b", Calls: Group{Recall: 0.5}}, {Name: "a"}, {Name: "b", Calls: Group{Recall: 0.9}}} {
		if err := write(out, r); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(out)
	var b Baseline
	if err := jsonUnmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Results) != 2 || b.Results[0].Name != "a" || b.Results[1].Calls.Recall != 0.9 {
		t.Fatalf("merged = %+v", b.Results)
	}
}

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// TestForwardDecl checks the scip-clang forward declaration test on
// prototypes and member declarations (true) and on calls (false).
func TestForwardDecl(t *testing.T) {
	cases := []struct {
		src  string // the name is marked with @
		ctor bool
		want bool
	}{
		{"CJSON_PUBLIC(cJSON *) @cJSON_Parse(const char *value);", false, true},
		{"/* Delete it. */\nCJSON_PUBLIC(void) @cJSON_Delete(cJSON *item);", false, true},
		{"static cJSON_bool\n@parse_value(cJSON * const item, parse_buffer * const input_buffer);", false, true},
		{"static int @f(int a,\n\tint b /* ; */);", false, true},
		{"class A {\npublic:\n    const char* @Name() const;", false, true},
		{"    virtual bool @Accept( XMLVisitor* visitor ) const = 0;", false, true},
		{"template <typename T, int N> T* @make(T (&a)[N]) noexcept;", false, true},
		{"protected:\n    @XMLNode( XMLDocument* );", true, true},
		{"    virtual ~@XMLNode();", true, true},
		{"extern \"C\" {\nint @run(void);", false, true},
		{"   return x;\n}\n*/\n\nCJSON_PUBLIC(cJSON *) @cJSONUtils_MergePatch(cJSON *target);", false, true},
		{"    @cJSON_Delete(item);", false, false},
		{"    @XMLNode(doc);", false, false},
		{"    return @f(x);", false, false},
		{"    x = a * @f(y);", false, false},
		{"    item = @cJSON_Parse(\"{\\\"a\\\": 1}\");", false, false},
		{"fail:\n    @cleanup(ctx);", false, false},
		{"    if (x) @f(y);", false, false},
		{"    else @f(y);", false, false},
		{"    (void)@f(y);", false, false},
		{"    g(@f(x));", false, false},
		{"    int n(@f(x));", false, false},
		{"    a->b(@f(x));", false, false},
		{"    int f(int x) { return @f(x - 1); }", false, false},
	}
	for _, c := range cases {
		lines := strings.Split(strings.Replace(c.src, "@", "", 1), "\n")
		line0, start := 0, 0
		for i, l := range strings.Split(c.src, "\n") {
			if j := strings.IndexByte(l, '@'); j >= 0 {
				line0, start = i, j
			}
		}
		end := start + len(identPrefix(strings.TrimPrefix(lines[line0][start:], "~")))
		if got := forwardDecl(lines, line0, start, end, c.ctor); got != c.want {
			t.Errorf("forwardDecl(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestCallableName(t *testing.T) {
	for _, c := range []struct{ symbol, name, owner string }{
		{"cxx . . $ cJSON_Parse(98ee06762456d062).", "cJSON_Parse", "$"},
		{"cxx . . $ tinyxml2/XMLDocument#XMLDocument(4bb3c7b5b9c3e8a1).", "XMLDocument", "XMLDocument"},
		{"cxx . . $ tinyxml2/XMLNode#`~XMLNode`(49f6e7a06ebc5aa8).", "~XMLNode", "XMLNode"},
		{"semanticdb maven maven/org.jsoup/jsoup 1.18.1 org/jsoup/nodes/Element#`<init>`(+1).", "<init>", "Element"},
	} {
		if name, owner := callableName(c.symbol); name != c.name || owner != c.owner {
			t.Errorf("callableName(%q) = %q, %q; want %q, %q", c.symbol, name, owner, c.name, c.owner)
		}
	}
	for symbol, want := range map[string]string{
		"semanticdb maven . . org/jsoup/nodes/Element#`<init>`(+1).":    "Element",
		"semanticdb maven . . org/jsoup/nodes/Element#attr(+1).":        "attr",
		"semanticdb maven . . org/jsoup/nodes/Document#OutputSettings#": "OutputSettings",
		"scip-dotnet nuget . . MediatR/Mediator#`.ctor`().":             "Mediator",
	} {
		if got := symbolName(symbol); got != want {
			t.Errorf("symbolName(%q) = %q, want %q", symbol, got, want)
		}
	}
	// An implicit constructor call sits on the variable's name.
	if r := clangNotReference("cxx . . $ tinyxml2/XMLDocument#XMLDocument(4bb3).", "doc", []string{"XMLDocument doc(true);"}, 0, 12, 15); r != "not the callable's name" {
		t.Errorf("implicit constructor call: %q", r)
	}
}

// TestEvaluateSCIPJava checks the scip-java conventions: imports carry no
// import role, and columns expand tabs to multiples of 8.
func TestEvaluateSCIPJava(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"p/A.java": "package p;\n\npublic class A {\n\tpublic static void f() {}\n}\n",
		"q/B.java": "package q;\n\nimport p.A;\n\npublic class B {\n\tvoid g() {\n\t\tA.f();\n\t\tA.f();\n\t}\n}\n",
	}
	for name, text := range files {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const a, f = "semanticdb maven . . p/A#", "semanticdb maven . . p/A#f()."
	var idx []byte
	for _, doc := range [][]byte{
		document("p/A.java", occurrence(2, 13, 14, a, roleDefinition), occurrence(3, 27, 28, f, roleDefinition)),
		// The import's A, and A.f() two tabs in: in expanded columns (16
		// and 18) on line 7 and in characters (2 and 4) on line 8.
		document("q/B.java", occurrence(2, 9, 10, a, 0), occurrence(6, 16, 17, a, 0), occurrence(6, 18, 19, f, 0),
			occurrence(7, 2, 3, a, 0), occurrence(7, 4, 5, f, 0)),
	} {
		idx = protowire.AppendTag(idx, 2, protowire.BytesType)
		idx = protowire.AppendBytes(idx, doc)
	}
	scipPath := filepath.Join(t.TempDir(), "index.scip")
	if err := os.WriteFile(scipPath, idx, 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := readSCIP(scipPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := evaluate(root, parsed, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Calls.Gold != 2 || res.Calls.Matched != 2 || res.Types.Gold != 2 {
		t.Fatalf("calls gold=%d matched=%d, types gold=%d; want 2, 2, 2 (%+v)", res.Calls.Gold, res.Calls.Matched, res.Types.Gold, res)
	}
}

func TestUntabAndTypeExpr(t *testing.T) {
	line := "        \thtml(sb);"
	if s, e, text := slice(line, untab(line, 16), untab(line, 20), encodingUnspecified); text != "html" || s != 9 || e != 13 {
		t.Fatalf("untab slice = %d %d %q", s, e, text)
	}
	if got := untab("no tabs", 3); got != 3 {
		t.Fatalf("untab without tabs = %d", got)
	}
	for text, want := range map[string]string{"KArgumentCaptor<T>": "KArgumentCaptor", "UseConstructor?": "UseConstructor", "Plain": "Plain"} {
		if m := typeExpr.FindStringSubmatch(text); m == nil || m[1] != want {
			t.Errorf("typeExpr(%q) = %v, want %q", text, m, want)
		}
	}
	if typeExpr.MatchString("t(\"\")).o") {
		t.Error("typeExpr matched an expression")
	}
}

// TestEvaluateSCIPKotlin checks the scip-java Kotlin conventions: a
// property read also references the accessor (getFirst().), which is not
// scored, and a type reference covers the whole type expression (A?).
func TestEvaluateSCIPKotlin(t *testing.T) {
	root := t.TempDir()
	text := "package p\n\nclass A(val first: Int) {\n    fun f() = first\n    fun g(a: A?) = a\n}\n"
	if err := os.WriteFile(filepath.Join(root, "A.kt"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	const a, get, first = "semanticdb maven . . p/A#", "semanticdb maven . . p/A#getFirst().", "semanticdb maven . . p/A#first."
	doc := document("A.kt",
		occurrence(2, 6, 7, a, roleDefinition), occurrence(2, 12, 17, first, roleDefinition), occurrence(2, 12, 17, get, roleDefinition),
		occurrence(3, 14, 19, first, 0), occurrence(3, 14, 19, get, 0),
		occurrence(4, 13, 15, a, 0))
	var idx []byte
	idx = protowire.AppendTag(idx, 2, protowire.BytesType)
	idx = protowire.AppendBytes(idx, doc)
	scipPath := filepath.Join(t.TempDir(), "index.scip")
	if err := os.WriteFile(scipPath, idx, 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := readSCIP(scipPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := evaluate(root, parsed, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Calls.Gold != 0 || res.Types.Gold != 1 || res.Types.Matched != 1 {
		t.Fatalf("calls gold=%d, types gold=%d matched=%d; want 0, 1, 1 (%+v)", res.Calls.Gold, res.Types.Gold, res.Types.Matched, res)
	}
}
