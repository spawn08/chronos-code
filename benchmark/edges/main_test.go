package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	res, err := evaluate(root, parsed, false)
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
