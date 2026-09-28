package scan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestIndexable(t *testing.T) {
	for rel, want := range map[string]bool{
		"a.go": true, "a/b/c.go": true, "a.bin": false, "a.txt": true, "CMakeLists.txt": false, "vendor/x.go": false, "a/node_modules/x.go": false,
		".hidden/x.go": false, "pkg/testdata/x.go": false, "a/.git/x.go": false,
		"app/main.py": true, "web/App.TSX": true, "src/lib.rs": true, "inc/x.h": true, "run.sh": true,
		"README.md": true, "docs/x.mdx": true, "api/users.proto": true, "api/openapi.yaml": true, "db/001_init.sql": true,
		"config.yaml": false, "Makefile": false, "node_modules/x/index.js": false, "vendor/a.rb": false,
	} {
		if got := Indexable(rel); got != want {
			t.Errorf("Indexable(%q) = %v", rel, got)
		}
	}
	if !IsModuleFile("go.mod") || !IsModuleFile("tools/go.mod") || IsModuleFile("vendor/go.mod") {
		t.Error("IsModuleFile")
	}
}

func TestListAndIgnoredOutsideRepoHonourGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree").Run(); err == nil {
		t.Skip("temp dir is inside a git repository")
	}
	for rel, body := range map[string]string{
		".gitignore":          "target/\n",
		"sub/.gitignore":      "*_gen.py\n",
		"src/lib.rs":          "fn a() {}\n",
		"target/debug/gen.rs": "fn b() {}\n",
		"sub/keep.py":         "x = 1\n",
		"sub/skip_gen.py":     "y = 2\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l, err := List(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(l.Sources)
	if want := []string{"src/lib.rs", "sub/keep.py"}; !slices.Equal(l.Sources, want) {
		t.Fatalf("Sources = %v, want %v", l.Sources, want)
	}
	ignored := Ignored(context.Background(), root, []string{"target/debug/gen.rs", "sub/skip_gen.py", "src/lib.rs"})
	if !ignored["target/debug/gen.rs"] || !ignored["sub/skip_gen.py"] || ignored["src/lib.rs"] {
		t.Fatalf("Ignored = %v", ignored)
	}
}

func TestModulePath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(p, []byte("// c\nmodule \"example.com/x\" // trailing\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ModulePath(p); got != "example.com/x" {
		t.Fatalf("ModulePath = %q", got)
	}
}
