package scan

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIndexable(t *testing.T) {
	for rel, want := range map[string]bool{
		"a.go": true, "a/b/c.go": true, "a.txt": false, "vendor/x.go": false, "a/node_modules/x.go": false,
		".hidden/x.go": false, "pkg/testdata/x.go": false, "a/.git/x.go": false,
	} {
		if got := Indexable(rel); got != want {
			t.Errorf("Indexable(%q) = %v", rel, got)
		}
	}
	if !IsModuleFile("go.mod") || !IsModuleFile("tools/go.mod") || IsModuleFile("vendor/go.mod") {
		t.Error("IsModuleFile")
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
