package claims

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "a.go", "package a\n\nfunc Parse() {\n\treturn\n}\n")
	writeFile(t, root, "b.go", "package b\n\nvar Budget = 3\n")
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func mustAdd(t *testing.T, s *Store, in Input) Claim {
	t.Helper()
	c, err := s.Add(in)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return c
}

func status(t *testing.T, s *Store, id string) Status {
	t.Helper()
	c, ok := s.Get(id)
	if !ok {
		t.Fatalf("claim %s missing", id)
	}
	return c.Status
}

func TestAddValidation(t *testing.T) {
	s, root := newTestStore(t)
	tests := []struct {
		name string
		in   Input
		want string
	}{
		{"empty text", Input{Text: "  ", Anchors: []Anchor{{Path: "a.go"}}}, "text is required"},
		{"no anchors", Input{Text: "x"}, "at least one anchor"},
		{"outside workspace", Input{Text: "x", Anchors: []Anchor{{Path: "../x.go"}}}, "outside the workspace"},
		{"missing file", Input{Text: "x", Anchors: []Anchor{{Path: "nope.go"}}}, "not readable"},
		{"bad range", Input{Text: "x", Anchors: []Anchor{{Path: "a.go", StartLine: 3, EndLine: 99}}}, "outside file"},
		{"unknown parent", Input{Text: "x", Anchors: []Anchor{{Path: "a.go"}}, DerivedFrom: []string{"c404"}}, "unknown claim"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Add(tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}

	c := mustAdd(t, s, Input{
		Text:        " Parse returns early ",
		Anchors:     []Anchor{{Path: filepath.Join(root, "a.go"), StartLine: 3, EndLine: 5, Symbol: "Parse"}},
		EvidenceID:  "ev1",
		DerivedFrom: nil,
	})
	if c.ID != "c1" || c.Text != "Parse returns early" || c.Status != StatusLive {
		t.Fatalf("unexpected claim %+v", c)
	}
	if c.Anchors[0].Path != "a.go" || c.Anchors[0].Hash == "" {
		t.Fatalf("anchor not normalized/hashed: %+v", c.Anchors[0])
	}
	dup := mustAdd(t, s, Input{Text: "y", Anchors: []Anchor{{Path: "b.go"}}, DerivedFrom: []string{"c1", "c1"}})
	if len(dup.DerivedFrom) != 1 {
		t.Fatalf("duplicate parents kept: %v", dup.DerivedFrom)
	}
}

func TestRefreshStaleCascadeAndRestore(t *testing.T) {
	s, root := newTestStore(t)
	parse := mustAdd(t, s, Input{Text: "Parse returns", Anchors: []Anchor{{Path: "a.go", StartLine: 3, EndLine: 5}}})
	budget := mustAdd(t, s, Input{Text: "Budget is 3", Anchors: []Anchor{{Path: "b.go", StartLine: 3, EndLine: 3}}})
	derived := mustAdd(t, s, Input{Text: "Parse is bounded by Budget", Anchors: []Anchor{{Path: "b.go", StartLine: 1, EndLine: 1}}, DerivedFrom: []string{parse.ID, budget.ID}})
	grand := mustAdd(t, s, Input{Text: "callers are bounded", Anchors: []Anchor{{Path: "b.go", StartLine: 1, EndLine: 1}}, DerivedFrom: []string{derived.ID}})

	original := "package a\n\nfunc Parse() {\n\treturn\n}\n"
	writeFile(t, root, "a.go", "package a\n\nfunc Parse() {\n\tpanic(1)\n}\n")
	changes := s.Refresh("a.go")
	want := []Change{
		{parse.ID, StatusLive, StatusStale},
		{derived.ID, StatusLive, StatusDoubted},
		{grand.ID, StatusLive, StatusDoubted},
	}
	if !equalChanges(changes, want) {
		t.Fatalf("changes = %+v, want %+v", changes, want)
	}
	if status(t, s, budget.ID) != StatusLive {
		t.Fatal("unrelated claim should stay live")
	}
	if c, _ := s.Get(parse.ID); !strings.Contains(c.Reason, "a.go:3-5 changed") {
		t.Fatalf("reason = %q", c.Reason)
	}
	if c, _ := s.Get(grand.ID); !strings.Contains(c.Reason, "doubted claim "+derived.ID) {
		t.Fatalf("reason = %q", c.Reason)
	}

	if got := s.Refresh("b.go"); len(got) != 0 {
		t.Fatalf("refreshing an unchanged file changed status: %+v", got)
	}

	writeFile(t, root, "a.go", original)
	changes = s.Refresh()
	if len(changes) != 3 || status(t, s, grand.ID) != StatusLive {
		t.Fatalf("restore failed: %+v", changes)
	}
	if c, _ := s.Get(parse.ID); c.Reason != "" {
		t.Fatalf("reason not cleared: %q", c.Reason)
	}
}

func TestRefreshRelocatesMovedSpan(t *testing.T) {
	s, root := newTestStore(t)
	c := mustAdd(t, s, Input{Text: "Parse returns", Anchors: []Anchor{{Path: "a.go", StartLine: 3, EndLine: 5}}})
	writeFile(t, root, "a.go", "package a\n\nimport \"fmt\"\n\nvar _ = fmt.Sprint\n\nfunc Parse() {\n\treturn\n}\n")
	if changes := s.Refresh(filepath.Join(root, "a.go")); len(changes) != 0 {
		t.Fatalf("moved span reported as change: %+v", changes)
	}
	got, _ := s.Get(c.ID)
	if got.Status != StatusLive || got.Anchors[0].StartLine != 7 || got.Anchors[0].EndLine != 9 {
		t.Fatalf("anchor not relocated: %+v", got)
	}
}

func TestRefreshDeletedFileAndMultipleAnchors(t *testing.T) {
	s, root := newTestStore(t)
	c := mustAdd(t, s, Input{Text: "spans both", Anchors: []Anchor{{Path: "a.go"}, {Path: "b.go", StartLine: 3, EndLine: 3}}})
	if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
		t.Fatal(err)
	}
	s.Refresh("b.go", "../ignored")
	got, _ := s.Get(c.ID)
	if got.Status != StatusStale || !strings.Contains(got.Reason, "b.go:3-3") {
		t.Fatalf("got %+v", got)
	}
}

func TestListAndGetReturnCopies(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, Input{Text: "one", Anchors: []Anchor{{Path: "a.go"}}})
	mustAdd(t, s, Input{Text: "two", Anchors: []Anchor{{Path: "b.go"}}, DerivedFrom: []string{"c1"}})
	list := s.List()
	if len(list) != 2 || list[0].ID != "c1" || list[1].ID != "c2" {
		t.Fatalf("list = %+v", list)
	}
	list[0].Anchors[0].Path = "mutated"
	list[1].DerivedFrom[0] = "mutated"
	again, _ := s.Get("c1")
	two, _ := s.Get("c2")
	if again.Anchors[0].Path != "a.go" || two.DerivedFrom[0] != "c1" {
		t.Fatal("store state was mutated through a returned copy")
	}
	if _, ok := s.Get("c9"); ok {
		t.Fatal("unexpected claim")
	}
}

func TestConcurrentAddAndRefresh(t *testing.T) {
	s, _ := newTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.Add(Input{Text: "x", Anchors: []Anchor{{Path: "a.go"}}}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			s.Refresh()
		}()
	}
	wg.Wait()
	if len(s.List()) != 8 {
		t.Fatalf("got %d claims", len(s.List()))
	}
}

func equalChanges(a, b []Change) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
