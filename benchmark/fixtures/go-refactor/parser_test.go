package parser

import "testing"

func TestParsers(t *testing.T) {
	want := []string{"a", "b"}
	for name, got := range map[string][]string{"header": ParseHeader("a, b"), "row": ParseRow("a, b")} {
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("%s parser = %v, want %v", name, got, want)
		}
	}
}
