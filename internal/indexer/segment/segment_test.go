package segment

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

func sampleFiles() []*facts.File {
	return []*facts.File{
		{
			Path: "b/b.go", Lang: "go", Package: "m/b", PkgName: "b", Hash: 2, Size: 20, MtimeNS: 200,
			Symbols: []facts.Symbol{
				{Name: "Run", Kind: facts.KindMethod, Receiver: "*T", Signature: "func (t *T) Run()", Line: 5, EndLine: 9, Exported: true},
				{Name: "T", Kind: facts.KindStruct, Signature: "type T struct", Doc: "T is.", Line: 3, EndLine: 3, Exported: true},
			},
			Imports: []facts.Import{{Path: "m/a", Line: 2}},
			Calls: []facts.Call{
				{Caller: 0, Callee: "Helper", Qualifier: "m/a", QualKind: facts.QualPackage, Line: 6, Col: 9},
				{Caller: 0, Callee: "step", Qualifier: "t", QualKind: facts.QualExpr, Line: 7, Col: 8},
			},
		},
		{
			Path: "a/a.go", Lang: "go", Package: "m/a", PkgName: "a", Hash: 1, Size: 10, MtimeNS: 100,
			Symbols: []facts.Symbol{{Name: "Helper", Kind: facts.KindFunc, Signature: "func Helper()", Line: 3, EndLine: 4, Exported: true}},
			Calls:   []facts.Call{{Caller: facts.NoCaller, Callee: "Helper", Line: 8, Col: 2}},
		},
		{Path: "gone.go", Deleted: true},
	}
}

func TestRoundTrip(t *testing.T) {
	data, err := Encode(sampleFiles(), KindBase, 7)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if s.Generation() != 7 || s.Kind() != KindBase || s.NumFiles() != 3 || s.NumSymbols() != 3 || s.NumCalls() != 3 {
		t.Fatalf("counts: gen=%d files=%d syms=%d calls=%d", s.Generation(), s.NumFiles(), s.NumSymbols(), s.NumCalls())
	}
	byPath := map[string]*facts.File{}
	for _, f := range sampleFiles() {
		byPath[f.Path] = f
	}
	for i := 0; i < s.NumFiles(); i++ {
		got := s.File(i)
		want := byPath[got.Path]
		if len(want.Symbols) > 1 { // decoded in line order
			want.Symbols[0], want.Symbols[1] = want.Symbols[1], want.Symbols[0]
			for k := range want.Calls {
				want.Calls[k].Caller = 1
			}
		}
		if !reflect.DeepEqual(normalize(got), normalize(want)) {
			t.Errorf("file %s:\n got %+v\nwant %+v", got.Path, got, want)
		}
	}
	if i, ok := s.FindFile("b/b.go"); !ok || s.FilePath(i) != "b/b.go" {
		t.Fatalf("FindFile b/b.go = %d %v", i, ok)
	}
	if _, ok := s.FindFile("nope.go"); ok {
		t.Fatal("FindFile found missing path")
	}
	if m := s.FileMeta(2); !m.Deleted || m.Path != "gone.go" {
		t.Fatalf("tombstone = %+v", m)
	}
	lo, hi := s.SymbolsNamed("Helper")
	if hi-lo != 1 || s.Symbol(lo).Kind != facts.KindFunc {
		t.Fatalf("SymbolsNamed Helper = [%d,%d)", lo, hi)
	}
	lo, hi = s.CallsTo("Helper")
	if hi-lo != 2 {
		t.Fatalf("CallsTo Helper = %d", hi-lo)
	}
	runLo, _ := s.SymbolsNamed("Run")
	from := s.CallsFrom(runLo)
	if len(from) != 2 || s.Call(from[0]).Callee != "Helper" || s.Call(from[1]).Callee != "step" {
		t.Fatalf("CallsFrom Run = %v", from)
	}
	if imps := s.Importers("m/a"); len(imps) != 1 || s.FilePath(s.Import(imps[0]).File) != "b/b.go" {
		t.Fatalf("Importers m/a = %v", imps)
	}
}

func normalize(f *facts.File) *facts.File {
	c := *f
	if len(c.Symbols) == 0 {
		c.Symbols = nil
	}
	if len(c.Imports) == 0 {
		c.Imports = nil
	}
	if len(c.Calls) == 0 {
		c.Calls = nil
	}
	return &c
}

func TestEmptySegment(t *testing.T) {
	data, err := Encode(nil, KindOverlay, 1)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(data)
	if err != nil || s.NumFiles() != 0 {
		t.Fatalf("empty: %v files=%d", err, s.NumFiles())
	}
	if lo, hi := s.SymbolsNamed("x"); lo != hi {
		t.Fatal("empty lookup returned results")
	}
}

// TestCorruptionDetected flips every byte of a segment in turn: each flip must
// be rejected with ErrCorrupt or leave a still-valid image (padding bytes).
func TestCorruptionDetected(t *testing.T) {
	data, err := Encode(sampleFiles(), KindBase, 1)
	if err != nil {
		t.Fatal(err)
	}
	rejected := 0
	for i := range data {
		bad := append([]byte(nil), data...)
		bad[i] ^= 0x5A
		if _, err := Parse(bad); err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("byte %d: error %v is not ErrCorrupt", i, err)
			}
			rejected++
		} else if i < 48 {
			t.Fatalf("byte %d in the header was not detected", i)
		}
	}
	if rejected < len(data)*9/10 {
		t.Fatalf("only %d/%d corruptions rejected", rejected, len(data))
	}
	if _, err := Parse(data[:len(data)-1]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncated image: %v", err)
	}
}

func TestOpenMapped(t *testing.T) {
	data, err := Encode(sampleFiles(), KindBase, 3)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "seg.chx")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.NumFiles() != 3 {
		t.Fatalf("files = %d", s.NumFiles())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicatePathRejected(t *testing.T) {
	if _, err := Encode([]*facts.File{{Path: "a.go"}, {Path: "a.go"}}, KindBase, 1); err == nil {
		t.Fatal("expected duplicate path error")
	}
}
