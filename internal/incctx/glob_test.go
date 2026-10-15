package incctx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/tool/builtins"
)

func TestWrapGlob(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{
		"backend/src/main.rs", "backend/target/debug/main.rs", "web/app/page.tsx", "web/app/util.ts",
		"web/node_modules/x/index.ts", "README.md", "app.log", "pkg/a_test.go", "pkg/a.go",
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Dir(p), filepath.Base(p), "x")
	}
	writeFile(t, dir, ".gitignore", "*.log\n")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "web/app/util.ts"), old, old); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t)
	a.Tools.Register(builtins.NewFileGlobTool(dir))
	WrapGlob(a, dir)
	tests := []struct {
		pattern, path string
		want          []string
	}{
		{"**/main.rs", "", []string{"backend/src/main.rs"}},
		{"**/*.{ts,tsx}", "", []string{"web/app/page.tsx", "web/app/util.ts"}}, // newest first
		{"*.md", "", []string{"README.md"}},
		{"**/*.log", "", nil},
		{"*_test.go", "pkg", []string{"pkg/a_test.go"}},
		{"**/nothing", "", nil},
	}
	for _, tt := range tests {
		args := map[string]any{"pattern": tt.pattern}
		if tt.path != "" {
			args["path"] = tt.path
		}
		out, err := a.Tools.Execute(context.Background(), "file_glob", args)
		if err != nil {
			t.Fatalf("%s: %v", tt.pattern, err)
		}
		res := out.(map[string]any)
		got := res["matches"].([]string)
		root, _ := filepath.EvalSymlinks(dir)
		var rels []string
		for _, m := range got {
			if r, err := filepath.EvalSymlinks(m); err == nil {
				m = r
			}
			rel, _ := filepath.Rel(root, m)
			rels = append(rels, filepath.ToSlash(rel))
		}
		if len(rels) != len(tt.want) {
			t.Errorf("%q: matches %v, want %v", tt.pattern, rels, tt.want)
			continue
		}
		for i := range rels {
			if rels[i] != tt.want[i] {
				t.Errorf("%q: matches %v, want %v", tt.pattern, rels, tt.want)
				break
			}
		}
	}
}
