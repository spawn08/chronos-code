package fsignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"**/main.rs", "backend/src/main.rs", true},
		{"**/main.rs", "main.rs", true},
		{"src/**/*.ts", "src/a/b/c.ts", true},
		{"src/**/*.ts", "src/c.ts", true},
		{"src/**/*.ts", "lib/c.ts", false},
		{"*.go", "a/b.go", false},
		{"**/*.{ts,tsx}", "web/app/page.tsx", true},
		{"**/*.{ts,tsx}", "web/app/page.js", false},
		{"**", "anything/at/all", true},
	}
	for _, tt := range tests {
		if got := Match(tt.pattern, tt.name); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestIgnored(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "*.log\n/build/\ndata/\n!keep.log\ncache/**\n!cache/keep.txt\n")
	write("frontend/.gitignore", "out/\n")
	m := New(root)
	tests := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"app.log", false, true},
		{"keep.log", false, false},
		{"sub/app.log", false, true},
		{"build", true, true},
		{"sub/build", true, false}, // anchored
		{"data", true, true},
		{"x/data", true, true}, // unanchored dir rule
		{"data", false, false}, // dir-only rule does not match a file
		{"frontend/out", true, true},
		{"out", true, false}, // nested rule applies only below its directory
		{"backend/target", true, true},
		{"web/node_modules", true, true},
		{"frontend/.next", true, true},
		{"src/main.rs", false, false},
		{"cache", true, false}, // dir/** matches inside dir, not dir
		{"cache/tmp.bin", false, true},
		{"cache/keep.txt", false, false}, // re-included by negation
		{"cache/sub/x", false, true},
	}
	for _, tt := range tests {
		if got := m.Ignored(filepath.Join(root, filepath.FromSlash(tt.rel)), tt.isDir); got != tt.want {
			t.Errorf("Ignored(%q, dir=%v) = %v, want %v", tt.rel, tt.isDir, got, tt.want)
		}
	}
	if m.Ignored(filepath.Join(filepath.Dir(root), "elsewhere.log"), false) {
		t.Error("paths outside the root must not be ignored")
	}
}
