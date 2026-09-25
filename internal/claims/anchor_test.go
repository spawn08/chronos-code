package claims

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnchor_String(t *testing.T) {
	tests := []struct {
		name string
		a    Anchor
		want string
	}{
		{name: "whole file", a: Anchor{Path: "a.go"}, want: "a.go"},
		{name: "span", a: Anchor{Path: "a.go", StartLine: 3, EndLine: 5}, want: "a.go:3-5"},
		{name: "span and symbol", a: Anchor{Path: "a.go", StartLine: 3, EndLine: 5, Symbol: "Parse"}, want: "a.go:3-5 Parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizePath(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "relative", path: "pkg/a.go", want: "pkg/a.go"},
		{name: "unclean relative", path: "pkg/../pkg/./a.go", want: "pkg/a.go"},
		{name: "absolute inside", path: filepath.Join(root, "pkg", "a.go"), want: "pkg/a.go"},
		{name: "empty", path: "  ", wantErr: true},
		{name: "root itself", path: ".", wantErr: true},
		{name: "escapes", path: "../a.go", wantErr: true},
		{name: "absolute outside", path: filepath.Join(filepath.Dir(root), "a.go"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizePath(root, tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizePath(%q) err = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("normalizePath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestFileLines(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\ntwo"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, ok := fileLines(root, "a.txt")
	if !ok || strings.Join(lines, "|") != "one|two" {
		t.Fatalf("fileLines = %q, %v", lines, ok)
	}
	if _, ok := fileLines(root, "missing.txt"); ok {
		t.Fatal("fileLines on missing file reported ok")
	}
}

func TestHashAnchor(t *testing.T) {
	lines := []string{"a", "b", "c"}
	whole, err := hashAnchor(lines, Anchor{Path: "f"})
	if err != nil || whole != hashSpan(lines, 1, 3) {
		t.Fatalf("whole-file hash = %q, %v", whole, err)
	}
	span, err := hashAnchor(lines, Anchor{Path: "f", StartLine: 2, EndLine: 2})
	if err != nil || span == whole {
		t.Fatalf("span hash = %q, %v", span, err)
	}
	for _, a := range []Anchor{
		{Path: "f", StartLine: 2, EndLine: 1},
		{Path: "f", StartLine: 1, EndLine: 4},
		{Path: "f", StartLine: -1, EndLine: 1},
	} {
		if _, err := hashAnchor(lines, a); err == nil {
			t.Fatalf("hashAnchor(%s) accepted invalid range", a)
		}
	}
	// Line boundaries matter: joining must not collide.
	if hashSpan([]string{"ab", "c"}, 1, 2) == hashSpan([]string{"a", "bc"}, 1, 2) {
		t.Fatal("hashSpan collides across line boundaries")
	}
}

func TestLocate(t *testing.T) {
	orig := []string{"package p", "", "func F() {", "\treturn", "}"}
	a := Anchor{Path: "p.go", StartLine: 3, EndLine: 5}
	a.Hash = hashSpan(orig, 3, 5)
	whole := Anchor{Path: "p.go", Hash: hashSpan(orig, 1, len(orig))}

	tests := []struct {
		name      string
		lines     []string
		anchor    Anchor
		wantOK    bool
		wantStart int
	}{
		{name: "unchanged", lines: orig, anchor: a, wantOK: true, wantStart: 3},
		{name: "shifted down", lines: append([]string{"// c", "// d"}, orig...), anchor: a, wantOK: true, wantStart: 5},
		{name: "shifted up", lines: orig[1:], anchor: a, wantOK: true, wantStart: 2},
		{name: "content changed", lines: []string{"package p", "", "func F() {", "\treturn 1", "}"}, anchor: a, wantOK: false, wantStart: 3},
		{name: "file gone", lines: nil, anchor: a, wantOK: false, wantStart: 3},
		{name: "truncated file", lines: orig[:2], anchor: a, wantOK: false, wantStart: 3},
		{name: "whole file unchanged", lines: orig, anchor: whole, wantOK: true},
		{name: "whole file changed", lines: append(orig, "// x"), anchor: whole, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := locate(tt.lines, tt.anchor)
			if ok != tt.wantOK {
				t.Fatalf("locate ok = %v, want %v", ok, tt.wantOK)
			}
			if got.StartLine != tt.wantStart {
				t.Fatalf("locate start = %d, want %d", got.StartLine, tt.wantStart)
			}
			if ok && !got.wholeFile() && got.EndLine-got.StartLine != tt.anchor.EndLine-tt.anchor.StartLine {
				t.Fatalf("locate changed span width: %s", got)
			}
		})
	}
}

func TestLocate_PrefersNearestDuplicate(t *testing.T) {
	lines := []string{"x", "}", "y", "z", "}", "w"}
	a := Anchor{Path: "f", StartLine: 4, EndLine: 4, Hash: hashSpan([]string{"}"}, 1, 1)}
	got, ok := locate(lines, a)
	if !ok || got.StartLine != 5 {
		t.Fatalf("locate = %s, %v; want nearest match at line 5", got, ok)
	}
}
