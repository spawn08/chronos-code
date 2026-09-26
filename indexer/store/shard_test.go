package store

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

func baseFiles(n int) []*facts.File {
	out := make([]*facts.File, n)
	for i := range out {
		out[i] = file(fmt.Sprintf("p%03d/f%05d.go", i%50, i), uint64(i+1), fmt.Sprintf("S%d", i))
	}
	return out
}

// checkModel compares every snapshot query with the expected live set.
func checkModel(t *testing.T, sn *Snapshot, model map[string]uint64) {
	t.Helper()
	if sn.NumFiles() != len(model) {
		t.Fatalf("NumFiles = %d, want %d", sn.NumFiles(), len(model))
	}
	got := livePaths(sn)
	want := slices.Sorted(maps.Keys(model))
	if !slices.Equal(got, want) {
		t.Fatalf("live paths differ: got %d, want %d", len(got), len(want))
	}
	for p, h := range model {
		m, ok := sn.Meta(p)
		if !ok || m.Hash != h {
			t.Fatalf("Meta(%s) = %v %v, want hash %d", p, m.Hash, ok, h)
		}
	}
	for _, p := range []string{"zz/missing.go", "p000/nope.go"} {
		if _, ok := sn.Lookup(p); ok {
			t.Fatalf("Lookup(%s) found a missing path", p)
		}
	}
	// Symbol liveness agrees with routing.
	live := 0
	for i := 0; i < sn.NumSegments(); i++ {
		seg := sn.Segment(i)
		for f := 0; f < seg.NumFiles(); f++ {
			if sn.Live(i, f) {
				live++
			}
		}
	}
	if live != len(model) {
		t.Fatalf("live records = %d, want %d", live, len(model))
	}
	for _, prefix := range []string{"p001/", "p049/", "new/", "p9"} {
		has := false
		for p := range model {
			has = has || strings.HasPrefix(p, prefix)
		}
		if sn.HasPrefix(prefix) != has {
			t.Fatalf("HasPrefix(%q) = %v, want %v", prefix, !has, has)
		}
	}
}

// Random overlays (edits, additions, deletions) and compactions against a
// small shard size, checked against a model after every step and after
// reopening.
func TestShardedRoutingAndCompactionModel(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	s.ShardBytes = 4096
	files := baseFiles(600)
	if err := s.Publish(files, true); err != nil {
		t.Fatal(err)
	}
	model := map[string]uint64{}
	for _, f := range files {
		model[f.Path] = f.Hash
	}
	sn := s.Snapshot()
	if sn.NumShards() < 5 {
		t.Fatalf("expected several shards, got %d", sn.NumShards())
	}
	sn.Release()
	rng := rand.New(rand.NewPCG(1, 2))
	hash := uint64(10000)
	for step := 0; step < 60; step++ {
		var batch []*facts.File
		for k := 0; k < 1+rng.IntN(20); k++ {
			hash++
			switch r := rng.IntN(10); {
			case r < 5: // edit an existing file
				p := files[rng.IntN(len(files))].Path
				batch = append(batch, file(p, hash, "E"))
				model[p] = hash
			case r < 8: // delete
				p := files[rng.IntN(len(files))].Path
				batch = append(batch, &facts.File{Path: p, Deleted: true})
				delete(model, p)
			default: // add a new file, sometimes before every shard bound
				p := fmt.Sprintf("new/n%05d.go", hash)
				if rng.IntN(3) == 0 {
					p = fmt.Sprintf("a%05d.go", hash)
				}
				batch = append(batch, file(p, hash, "N"))
				model[p] = hash
			}
		}
		// Within one overlay the last record for a path wins.
		byPath := map[string]*facts.File{}
		for _, f := range batch {
			byPath[f.Path] = f
		}
		if err := s.Publish(slices.Collect(maps.Values(byPath)), false); err != nil {
			t.Fatal(err)
		}
		if step%7 == 6 {
			if err := s.Compact(); err != nil {
				t.Fatal(err)
			}
		}
		sn := s.Snapshot()
		checkModel(t, sn, model)
		sn.Release()
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir)
	defer s.Close()
	sn = s.Snapshot()
	checkModel(t, sn, model)
	sn.Release()
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	sn = s.Snapshot()
	defer sn.Release()
	if sn.NumSegments() != sn.NumShards() {
		t.Fatal("overlays left after compaction")
	}
	checkModel(t, sn, model)
}

// Compaction rewrites only the shard an overlay touches.
func TestCompactionRewritesOnlyTouchedShards(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer s.Close()
	s.ShardBytes = 4096
	files := baseFiles(600)
	if err := s.Publish(files, true); err != nil {
		t.Fatal(err)
	}
	before := s.Manifest().Segments
	edited := files[len(files)/2].Path
	if err := s.Publish([]*facts.File{file(edited, 99, "X")}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	after := s.Manifest().Segments
	kept := 0
	for _, e := range after {
		if slices.ContainsFunc(before, func(b SegmentEntry) bool { return b.Name == e.Name }) {
			kept++
		}
	}
	if kept != len(before)-1 || len(after) < len(before) {
		t.Fatalf("compaction rewrote %d of %d shards", len(before)-kept, len(before))
	}
	sn := s.Snapshot()
	defer sn.Release()
	if m, _ := sn.Meta(edited); m.Hash != 99 {
		t.Fatalf("edit lost in compaction: %d", m.Hash)
	}
}

// The edit path's routing work depends on the overlay, not the base: the
// same one-file overlay over a 10x larger base touches the same number of
// entries.
func TestEditPathWorkIndependentOfBaseSize(t *testing.T) {
	work := func(n int) int {
		s := openStore(t, t.TempDir())
		defer s.Close()
		if err := s.Publish(baseFiles(n), true); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ { // a few earlier edits
			if err := s.Publish([]*facts.File{file(fmt.Sprintf("p001/f%05d.go", 1+50*i), 500+uint64(i), "E")}, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Publish([]*facts.File{file("p007/f00007.go", 999, "E")}, false); err != nil {
			t.Fatal(err)
		}
		sn := s.Snapshot()
		defer sn.Release()
		return sn.BuildWork()
	}
	small, large := work(500), work(5000)
	if small != large || small > 10 {
		t.Fatalf("edit work: %d entries at 500 files, %d at 5000", small, large)
	}
}

func TestBaseWriterRejectsOutOfOrderAndAbortCleans(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	defer s.Close()
	s.ShardBytes = 256
	w := s.NewBase()
	if err := w.Add(file("b.go", 1, "B")); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(file("a.go", 2, "A")); err == nil {
		t.Fatal("out-of-order path accepted")
	}
	w.Abort()
	if err := s.Publish([]*facts.File{file("c.go", 3, "C")}, true); err != nil {
		t.Fatalf("store unusable after Abort: %v", err)
	}
	s.cleanOrphans()
	if len(s.Manifest().Segments) != 1 {
		t.Fatalf("segments = %+v", s.Manifest().Segments)
	}
}

func TestMetaPersistsAndImport(t *testing.T) {
	src := t.TempDir()
	s := openStore(t, src)
	if err := s.Publish(baseFiles(40), true); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish([]*facts.File{file("p001/f00001.go", 77, "E")}, false); err != nil {
		t.Fatal(err)
	}
	meta := Meta{Complete: true, Commit: "abc", Touched: []string{"x.go"}, Modules: map[string]string{".": "m"}}
	if err := s.SetMeta(meta); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openStore(t, src)
	if got := s.Meta(); got.Commit != "abc" || !got.Complete || len(got.Touched) != 1 || got.Modules["."] != "m" {
		t.Fatalf("meta after reopen: %+v", got)
	}
	s.Close()

	dst, err := Open(t.TempDir(), "/elsewhere", "go-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.Import(src); err != nil {
		t.Fatal(err)
	}
	sn := dst.Snapshot()
	defer sn.Release()
	if m, _ := sn.Meta("p001/f00001.go"); sn.NumFiles() != 40 || m.Hash != 77 || dst.Meta().Commit != "abc" || dst.Manifest().Root != "/elsewhere" {
		t.Fatalf("import: files=%d hash=%d meta=%+v", sn.NumFiles(), m.Hash, dst.Meta())
	}
	other, err := Open(t.TempDir(), "/r", "other-extractor")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Import(src); err == nil {
		t.Fatal("import across extractor versions accepted")
	}
}
