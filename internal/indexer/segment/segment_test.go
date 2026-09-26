package segment

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cespare/xxhash/v2"

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
			Refs: []facts.Ref{
				{Enclosing: 0, Name: "Helper", Qualifier: "m/a", QualKind: facts.QualPackage, Line: 6, Col: 9},
				{Enclosing: 0, Name: "step", Qualifier: "t", QualKind: facts.QualExpr, Line: 7, Col: 8},
			},
		},
		{
			Path: "a/a.go", Lang: "go", Package: "m/a", PkgName: "a", Hash: 1, Size: 10, MtimeNS: 100,
			Symbols: []facts.Symbol{{Name: "Helper", Kind: facts.KindFunc, Signature: "func Helper()", Line: 3, EndLine: 4, Exported: true}},
			Refs:    []facts.Ref{{Enclosing: facts.NoCaller, Name: "Helper", Line: 8, Col: 2}},
		},
		{Path: "gone.go", Deleted: true},
		v2File(),
	}
}

// v2File uses every language-neutral field of facts v2.
func v2File() *facts.File {
	return &facts.File{
		Path: "web/svc.ts", Lang: "typescript", Package: "web", Hash: 3, Size: 30, MtimeNS: 300,
		Generated: true, Test: true, Vendored: true,
		Symbols: []facts.Symbol{
			{Name: "Svc", Kind: facts.KindClass, Signature: "export class Svc extends Base", Line: 3, EndLine: 9,
				Exported: true, Visibility: facts.VisPublic, Modifiers: facts.ModAbstract},
			{Name: "save", Kind: facts.KindMethod, Receiver: "Svc", Signature: "async save(r: Repo, ...rest)", Line: 5, EndLine: 7,
				Container: 1, Visibility: facts.VisProtected, Modifiers: facts.ModAsync | facts.ModOverride,
				Params: facts.Arity{Min: 1, Max: facts.VarArgs, Known: true}, ParamList: "(r: Repo, ...rest)"},
			{Name: "put", Kind: facts.KindMethod, Receiver: "Svc", Signature: "put(k, v = 0)", Line: 8, EndLine: 8,
				Container: 1, Params: facts.Arity{Min: 1, Max: 2, Known: true}},
		},
		Imports: []facts.Import{
			{Path: "./repo", Line: 1, Kind: facts.ImportModule, Names: []facts.ImportedName{{Name: "Repo"}, {Name: "Base", Alias: "B"}}},
			{Path: "./util", Line: 2, Kind: facts.ImportWildcard, Name: "u"},
		},
		Refs: []facts.Ref{
			{Kind: facts.RefExtends, Enclosing: 0, Name: "Base", Line: 3, Col: 26},
			{Kind: facts.RefTypeUse, Enclosing: 1, Name: "Repo", Line: 5, Col: 16},
			{Kind: facts.RefCall, Enclosing: 1, Name: "put", Qualifier: "r", QualKind: facts.QualExpr, Line: 6, Col: 7, Args: 3, ArgTypes: "k,#n"},
			{Kind: facts.RefDecorator, Enclosing: 1, Name: "logged", Line: 4, Col: 3, Lambda: 3},
		},
		Exports: []facts.Export{
			{Name: "*", Source: "./util", Line: 10},
			{Name: "Store", Source: "./repo", SourceName: "Repo", Line: 11},
		},
		Hints: []facts.BindingHint{
			{Scope: 1, Name: "r", Type: "Repo", Line: 5},
			{Scope: facts.NoCaller, Name: "svc", Type: "Svc", Line: 12},
		},
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
	if s.Generation() != 7 || s.Kind() != KindBase || s.NumFiles() != 4 || s.NumSymbols() != 6 || s.NumRefs() != 7 {
		t.Fatalf("counts: gen=%d files=%d syms=%d refs=%d", s.Generation(), s.NumFiles(), s.NumSymbols(), s.NumRefs())
	}
	byPath := map[string]*facts.File{}
	for _, f := range sampleFiles() {
		byPath[f.Path] = f
	}
	for i := 0; i < s.NumFiles(); i++ {
		got := s.File(i)
		want := byPath[got.Path]
		if want.Path == "b/b.go" { // decoded in line order
			want.Symbols[0], want.Symbols[1] = want.Symbols[1], want.Symbols[0]
			for k := range want.Refs {
				want.Refs[k].Enclosing = 1
			}
		}
		if want.Path == "web/svc.ts" { // refs decoded in source order
			r := want.Refs
			want.Refs = []facts.Ref{r[0], r[3], r[1], r[2]}
			want.Refs[1].Lambda = 4 // input ref 2 is decoded as ref 3
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
	if m := s.FileMeta(2); !m.Deleted || m.Path != "gone.go" || m.Test || m.Generated {
		t.Fatalf("tombstone = %+v", m)
	}
	lo, hi := s.SymbolsNamed("Helper")
	if hi-lo != 1 || s.Symbol(lo).Kind != facts.KindFunc {
		t.Fatalf("SymbolsNamed Helper = [%d,%d)", lo, hi)
	}
	lo, hi = s.RefsTo("Helper")
	if hi-lo != 2 {
		t.Fatalf("RefsTo Helper = %d", hi-lo)
	}
	runLo, _ := s.SymbolsNamed("Run")
	from := s.RefsFrom(runLo)
	if len(from) != 2 || s.Ref(from[0]).Name != "Helper" || s.Ref(from[1]).Name != "step" {
		t.Fatalf("RefsFrom Run = %v", from)
	}
	if i, _ := s.FindFile("web/svc.ts"); !s.FileMeta(i).Test || !s.FileMeta(i).Generated || !s.FileMeta(i).Vendored {
		t.Fatalf("file flags lost: %+v", s.FileMeta(i))
	}
	lo, _ = s.RefsTo("Base")
	if s.RefKind(lo) != facts.RefExtends || s.Ref(lo).Kind != facts.RefExtends {
		t.Fatalf("ref kind = %d", s.RefKind(lo))
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
	if len(c.Refs) == 0 {
		c.Refs = nil
	}
	if len(c.Exports) == 0 {
		c.Exports = nil
	}
	if len(c.Hints) == 0 {
		c.Hints = nil
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
	if s.NumFiles() != 4 {
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

func TestContainerRoundTrip(t *testing.T) {
	files := []*facts.File{{
		Path: "i.go", Lang: "go", Package: "m", PkgName: "m",
		Symbols: []facts.Symbol{
			{Name: "Save", Kind: facts.KindMethod, Receiver: "Repo", Line: 4, EndLine: 4, Container: 3},
			{Name: "Closer", Kind: facts.KindEmbed, Signature: "io.Closer", Line: 3, EndLine: 3, Container: 3},
			{Name: "Repo", Kind: facts.KindInterface, Line: 2, EndLine: 5},
			{Name: "Free", Kind: facts.KindFunc, Line: 7, EndLine: 7},
		},
	}}
	data, err := Encode(files, KindBase, 1)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	parents := map[string]string{}
	for k := 0; k < s.NumSymbols(); k++ {
		rec := s.Symbol(k)
		if rec.Container != 0 {
			t.Fatalf("SymbolRec must not carry a file-local Container: %+v", rec)
		}
		if rec.Parent >= 0 {
			parents[rec.Name] = s.Symbol(rec.Parent).Name
		}
		if s.SymbolParent(k) != rec.Parent || s.SymbolKind(k) != rec.Kind || s.SymbolName(k) != rec.Name {
			t.Fatalf("accessors disagree for %+v", rec)
		}
	}
	if parents["Save"] != "Repo" || parents["Closer"] != "Repo" || len(parents) != 2 {
		t.Fatalf("parents = %v", parents)
	}
	// Decoded files are in line order; containers must follow the reorder.
	f := s.File(0)
	for _, sym := range f.Symbols {
		p, ok := sym.Parent()
		switch sym.Name {
		case "Save", "Closer":
			if !ok || f.Symbols[p].Name != "Repo" {
				t.Fatalf("decoded %s parent = %d (%v)", sym.Name, p, ok)
			}
		default:
			if ok {
				t.Fatalf("decoded %s must be top level", sym.Name)
			}
		}
	}
	// A container pointing at a symbol of another file is corrupt.
	two := []*facts.File{
		{Path: "a.go", Symbols: []facts.Symbol{{Name: "A", Kind: facts.KindFunc, Line: 1}}},
		{Path: "b.go", Symbols: []facts.Symbol{{Name: "B", Kind: facts.KindFunc, Line: 1}}},
	}
	data, err = Encode(two, KindBase, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatal(err)
	}
	// Point B (symbol 1, file 1) at A (symbol 0, file 0) and re-checksum.
	b := append([]byte(nil), data...)
	resum(b)
	if _, err := Parse(b); err != nil {
		t.Fatalf("resum must keep an untampered image valid: %v", err)
	}
	symOff := int(le.Uint64(b[le.Uint64(b[24:])+uint64(secSymbols-1)*sectionEntry+8:]))
	le.PutUint32(b[symOff+symbolRecSize+48:], 0)
	resum(b)
	if _, err := Parse(b); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("cross-file container accepted: %v", err)
	}
}

// resum recomputes section, table and header checksums after a test edit.
func resum(b []byte) {
	tableOff := le.Uint64(b[24:])
	for i := 0; i < numSections; i++ {
		e := b[tableOff+uint64(i*sectionEntry):]
		off, n := le.Uint64(e[8:]), le.Uint64(e[16:])
		le.PutUint64(e[24:], xxhash.Sum64(b[off:off+n]))
	}
	le.PutUint64(b[32:], xxhash.Sum64(b[tableOff:]))
	le.PutUint64(b[40:], xxhash.Sum64(b[:40]))
}

// sectionOff returns the offset of section sec in a segment image.
func sectionOff(b []byte, sec int) int {
	return int(le.Uint64(b[le.Uint64(b[24:])+uint64(sec-1)*sectionEntry+8:]))
}

// TestV2FieldsValidated tampers with each v2 field that Parse must check and
// re-checksums, so only semantic validation can reject the image.
func TestV2FieldsValidated(t *testing.T) {
	two := []*facts.File{
		{Path: "a.ts", Symbols: []facts.Symbol{{Name: "A", Kind: facts.KindClass, Line: 1}}},
		{
			Path:    "b.ts",
			Symbols: []facts.Symbol{{Name: "B", Kind: facts.KindClass, Line: 1}},
			Imports: []facts.Import{{Path: "./a", Line: 1, Names: []facts.ImportedName{{Name: "A"}}}},
			Refs:    []facts.Ref{{Kind: facts.RefExtends, Enclosing: 0, Name: "A", Line: 1}},
			Hints:   []facts.BindingHint{{Scope: 0, Name: "x", Type: "A", Line: 1}},
		},
	}
	data, err := Encode(two, KindBase, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(b []byte){
		"unknown visibility": func(b []byte) { b[sectionOff(b, secSymbols)+46] = facts.NumVisibility },
		"unknown ref kind":   func(b []byte) { b[sectionOff(b, secRefs)+31] = facts.NumRefKinds },
		// Symbols sort by name: A (file 0) is symbol 0, B (file 1) symbol 1.
		"cross-file ref enclosing": func(b []byte) { le.PutUint32(b[sectionOff(b, secRefs)+20:], 0) },
		"cross-file hint scope":    func(b []byte) { le.PutUint32(b[sectionOff(b, secHints)+20:], 0) },
		"unknown import kind":      func(b []byte) { b[sectionOff(b, secImports)+24] = facts.NumImportKinds },
		"import names out of range": func(b []byte) {
			le.PutUint32(b[sectionOff(b, secImports)+32:], 2)
		},
		"file hint range out of range": func(b []byte) {
			le.PutUint32(b[sectionOff(b, secFiles)+fileRecSize+108:], 5)
		},
		"arity above maximum": func(b []byte) {
			b[sectionOff(b, secSymbols)+45] |= flagArity
			b[sectionOff(b, secSymbols)+56] = facts.MaxArity + 1
			b[sectionOff(b, secSymbols)+57] = varArgs
		},
		"lambda in another file": func(b []byte) {
			le.PutUint32(b[sectionOff(b, secRefs)+44:], 0) // ref 0 is the only ref: itself
		},
		"arity max below min": func(b []byte) {
			b[sectionOff(b, secSymbols)+45] |= flagArity
			b[sectionOff(b, secSymbols)+56] = 3
			b[sectionOff(b, secSymbols)+57] = 2
		},
	} {
		b := append([]byte(nil), data...)
		edit(b)
		resum(b)
		if _, err := Parse(b); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

func TestEncodeRejectsUnknownValues(t *testing.T) {
	for name, f := range map[string]*facts.File{
		"visibility":  {Path: "a", Symbols: []facts.Symbol{{Name: "A", Kind: facts.KindFunc, Visibility: facts.NumVisibility}}},
		"ref kind":    {Path: "a", Refs: []facts.Ref{{Name: "f", Kind: facts.NumRefKinds, Enclosing: facts.NoCaller}}},
		"import kind": {Path: "a", Imports: []facts.Import{{Path: "x", Kind: facts.NumImportKinds}}},
		"symbol kind": {Path: "a", Symbols: []facts.Symbol{{Name: "A", Kind: "gadget"}}},
		"arity":       {Path: "a", Symbols: []facts.Symbol{{Name: "A", Kind: facts.KindFunc, Params: facts.Arity{Min: 2, Max: 1, Known: true}}}},
		"large arity": {Path: "a", Symbols: []facts.Symbol{{Name: "A", Kind: facts.KindFunc, Params: facts.Arity{Min: facts.MaxArity + 1, Max: facts.VarArgs, Known: true}}}},
	} {
		if _, err := Encode([]*facts.File{f}, KindBase, 1); err == nil {
			t.Errorf("%s: encode accepted an unknown value", name)
		}
	}
}

// TestEncodeDeterministic checks that equal inputs produce identical bytes,
// which portable (prebuilt) segments rely on.
func TestEncodeDeterministic(t *testing.T) {
	a, err := Encode(sampleFiles(), KindBase, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		b, err := Encode(sampleFiles(), KindBase, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatal("encoding is not deterministic")
		}
	}
}
