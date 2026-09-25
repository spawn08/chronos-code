package claims

import (
	"strings"
	"testing"
)

func TestRestoreRoundTripThenRefresh(t *testing.T) {
	s, root := newTestStore(t)
	parent := mustAdd(t, s, Input{Text: "Parse returns", Anchors: []Anchor{{Path: "a.go", StartLine: 3, EndLine: 5}}})
	child := mustAdd(t, s, Input{Text: "Budget used by Parse", Anchors: []Anchor{{Path: "b.go", StartLine: 3, EndLine: 3}}, DerivedFrom: []string{parent.ID}})

	restored, err := Restore(root, s.List())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if status(t, restored, parent.ID) != StatusLive || status(t, restored, child.ID) != StatusLive {
		t.Fatal("restored claims should be live")
	}
	next := mustAdd(t, restored, Input{Text: "package b", Anchors: []Anchor{{Path: "b.go", StartLine: 1, EndLine: 1}}})
	if next.ID == parent.ID || next.ID == child.ID {
		t.Fatalf("new claim reused restored id %s", next.ID)
	}

	writeFile(t, root, "a.go", "package a\n\nfunc Parse() {\n\tpanic(1)\n}\n")
	restored.Refresh()
	if got := status(t, restored, parent.ID); got != StatusStale {
		t.Fatalf("parent = %s, want stale", got)
	}
	if got := status(t, restored, child.ID); got != StatusDoubted {
		t.Fatalf("child = %s, want doubted", got)
	}
}

func TestRestoreRejectsInvalidSnapshots(t *testing.T) {
	s, root := newTestStore(t)
	good := mustAdd(t, s, Input{Text: "Parse", Anchors: []Anchor{{Path: "a.go", StartLine: 3, EndLine: 5}}})

	mutate := func(f func(*Claim)) []Claim {
		c, _ := s.Get(good.ID)
		f(&c)
		return []Claim{c}
	}
	cases := map[string]struct {
		saved []Claim
		want  string
	}{
		"duplicate id":   {append(s.List(), s.List()...), "duplicate id"},
		"malformed id":   {mutate(func(c *Claim) { c.ID = "x1" }), "malformed id"},
		"missing hash":   {mutate(func(c *Claim) { c.Anchors[0].Hash = "" }), "no hash"},
		"unknown status": {mutate(func(c *Claim) { c.Status = "bogus" }), "unknown status"},
		"no anchors":     {mutate(func(c *Claim) { c.Anchors = nil }), "anchors are required"},
		"forward parent": {mutate(func(c *Claim) { c.DerivedFrom = []string{"c9"} }), "unknown or later claim"},
		"escaping path":  {mutate(func(c *Claim) { c.Anchors[0].Path = "../outside.go" }), ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Restore(root, tc.saved)
			if err == nil {
				t.Fatal("Restore accepted an invalid snapshot")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
