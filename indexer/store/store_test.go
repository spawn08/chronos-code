package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/indexer/facts"
)

func file(path string, hash uint64, syms ...string) *facts.File {
	f := &facts.File{Path: path, Lang: "go", Package: "m/" + filepath.Dir(path), Hash: hash}
	for i, s := range syms {
		f.Symbols = append(f.Symbols, facts.Symbol{Name: s, Kind: facts.KindFunc, Line: i + 1, EndLine: i + 1})
	}
	return f
}

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, "/root", "go-v1")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func livePaths(sn *Snapshot) []string {
	p := sn.Paths()
	sort.Strings(p)
	return p
}

func TestPublishOverlayAndReopen(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	if err := s.Publish([]*facts.File{file("a.go", 1, "A"), file("b.go", 2, "B")}, true); err != nil {
		t.Fatal(err)
	}
	old := s.Snapshot()
	if err := s.Publish([]*facts.File{file("a.go", 3, "A2"), {Path: "b.go", Deleted: true}, file("c.go", 4, "C")}, false); err != nil {
		t.Fatal(err)
	}
	sn := s.Snapshot()
	if got := strings.Join(livePaths(sn), ","); got != "a.go,c.go" {
		t.Fatalf("live = %s", got)
	}
	if m, _ := sn.Meta("a.go"); m.Hash != 3 {
		t.Fatalf("a.go hash = %d", m.Hash)
	}
	if sn.Generation() != 2 || sn.NumSegments() != 2 || sn.NumFiles() != 2 {
		t.Fatalf("gen=%d segs=%d files=%d", sn.Generation(), sn.NumSegments(), sn.NumFiles())
	}
	// The older snapshot is unaffected.
	if got := strings.Join(livePaths(old), ","); got != "a.go,b.go" {
		t.Fatalf("old snapshot live = %s", got)
	}
	old.Release()
	sn.Release()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openStore(t, dir)
	defer s.Close()
	sn = s.Snapshot()
	defer sn.Release()
	if s.Recovered != "" || sn.Generation() != 2 || strings.Join(livePaths(sn), ",") != "a.go,c.go" {
		t.Fatalf("reopen: recovered=%q gen=%d live=%v", s.Recovered, sn.Generation(), livePaths(sn))
	}
}

func TestCompactMergesAndRemovesOldSegments(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	defer s.Close()
	if err := s.Publish([]*facts.File{file("a.go", 1, "A"), file("b.go", 2, "B")}, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.Publish([]*facts.File{file("a.go", uint64(10+i), "A")}, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Publish([]*facts.File{{Path: "b.go", Deleted: true}}, false); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := s.OverlayStats(); n != 6 {
		t.Fatalf("overlays = %d", n)
	}
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	sn := s.Snapshot()
	defer sn.Release()
	if sn.NumSegments() != 1 || strings.Join(livePaths(sn), ",") != "a.go" {
		t.Fatalf("after compact: segs=%d live=%v", sn.NumSegments(), livePaths(sn))
	}
	if m, _ := sn.Meta("a.go"); m.Hash != 14 {
		t.Fatalf("a.go hash = %d", m.Hash)
	}
	segs, _ := filepath.Glob(filepath.Join(dir, segPrefix+"*"))
	if len(segs) != 1 {
		t.Fatalf("segment files after compaction = %v", segs)
	}
}

// TestCompactionPreservesV2Facts checks that compaction, which decodes files
// and re-encodes them, keeps every facts v2 field.
func TestCompactionPreservesV2Facts(t *testing.T) {
	rich := func() *facts.File {
		return &facts.File{
			Path: "web/svc.ts", Lang: "typescript", Package: "web", Hash: 9, Generated: true, Test: true, Vendored: true,
			Symbols: []facts.Symbol{
				{Name: "Svc", Kind: facts.KindClass, Line: 1, EndLine: 9, Exported: true, Visibility: facts.VisPublic, Modifiers: facts.ModAbstract},
				{Name: "save", Kind: facts.KindMethod, Receiver: "Svc", Line: 3, EndLine: 5, Container: 1,
					Visibility: facts.VisPrivate, Modifiers: facts.ModAsync | facts.ModStatic},
			},
			Imports: []facts.Import{{Path: "./repo", Line: 1, Kind: facts.ImportModule, Names: []facts.ImportedName{{Name: "Repo", Alias: "R"}}}},
			Refs: []facts.Ref{
				{Kind: facts.RefExtends, Enclosing: 0, Name: "Base", Line: 1, Col: 20},
				{Kind: facts.RefCall, Enclosing: 1, Name: "put", Qualifier: "r", QualKind: facts.QualExpr, Line: 4, Col: 3},
			},
			Exports: []facts.Export{{Name: "Store", Source: "./repo", SourceName: "Repo", Line: 10}},
			Hints:   []facts.BindingHint{{Scope: 1, Name: "r", Type: "Repo", Line: 3}},
		}
	}
	dir := t.TempDir()
	s := openStore(t, dir)
	defer s.Close()
	if err := s.Publish([]*facts.File{rich(), file("a.go", 1, "A")}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish([]*facts.File{file("a.go", 2, "A2")}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	sn := s.Snapshot()
	defer sn.Release()
	if sn.NumSegments() != 1 {
		t.Fatalf("segments after compaction = %d", sn.NumSegments())
	}
	// The only segment holds the overlay's a.go, so the shard with
	// web/svc.ts was decoded and re-encoded, not kept.
	if m, _ := sn.Meta("a.go"); m.Hash != 2 {
		t.Fatalf("a.go hash = %d, want the overlay's 2", m.Hash)
	}
	ref, ok := sn.Lookup("web/svc.ts")
	if !ok {
		t.Fatal("web/svc.ts lost")
	}
	got, want := sn.Segment(int(ref.Seg)).File(int(ref.File)), rich()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("v2 facts changed by compaction:\n got %+v\nwant %+v", got, want)
	}
}

func TestCrashBeforeManifestKeepsPreviousGeneration(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	if err := s.Publish([]*facts.File{file("a.go", 1, "A")}, true); err != nil {
		t.Fatal(err)
	}
	crash := errors.New("simulated crash")
	s.beforeManifest = func() error { return crash }
	if err := s.Publish([]*facts.File{file("a.go", 2, "A")}, false); !errors.Is(err, crash) {
		t.Fatalf("publish err = %v", err)
	}
	sn := s.Snapshot()
	if m, _ := sn.Meta("a.go"); m.Hash != 1 || sn.Generation() != 1 {
		t.Fatalf("in-process view changed: gen=%d hash=%d", sn.Generation(), m.Hash)
	}
	sn.Release()
	_ = s.unlock() // simulate process death: no Close, orphan segment left behind

	s = openStore(t, dir)
	defer s.Close()
	sn = s.Snapshot()
	defer sn.Release()
	if m, _ := sn.Meta("a.go"); m.Hash != 1 || sn.Generation() != 1 || s.Recovered != "" {
		t.Fatalf("after restart: gen=%d hash=%d recovered=%q", sn.Generation(), m.Hash, s.Recovered)
	}
	segs, _ := filepath.Glob(filepath.Join(dir, segPrefix+"*"))
	if len(segs) != 1 {
		t.Fatalf("orphan segment not cleaned: %v", segs)
	}
}

func TestCorruptSegmentResetsIndex(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	if err := s.Publish([]*facts.File{file("a.go", 1, "A")}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	segs, _ := filepath.Glob(filepath.Join(dir, segPrefix+"*"))
	data, _ := os.ReadFile(segs[0])
	data[len(data)/2] ^= 0xFF
	if err := os.WriteFile(segs[0], data, 0o644); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir)
	defer s.Close()
	sn := s.Snapshot()
	defer sn.Release()
	if s.Recovered == "" || sn.Generation() != 0 || sn.NumFiles() != 0 {
		t.Fatalf("corrupt index not reset: recovered=%q gen=%d", s.Recovered, sn.Generation())
	}
}

func TestForeignIndexDiscarded(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	if err := s.Publish([]*facts.File{file("a.go", 1, "A")}, true); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s2, err := Open(dir, "/root", "go-v2")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Recovered == "" {
		t.Fatal("extractor change did not discard the index")
	}
}

func TestWriterLockExclusive(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	defer s.Close()
	if _, err := Open(dir, "/root", "go-v1"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second open err = %v, want ErrLocked", err)
	}
}
