package incctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// TestGrepBackendsAgree runs the same searches through ripgrep and the Go
// walker over a fixture with ignore rules, ecosystem directories, hidden
// directories and binary files; both must return identical files.
func TestGrepBackendsAgree(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":               "*.log\ndata/\n",
		"src/app.py":               "def needle(): pass\n",
		"src/view.tsx":             "export const Needle = 1 // needle\n",
		"src/debug.log":            "needle\n",
		"data/cache.txt":           "needle\n",
		"backend/target/out.txt":   "needle\n",
		"frontend/.next/page.js":   "needle\n",
		"frontend/node_modules/x":  "needle\n",
		".github/workflows/ci.yml": "run: needle\n",
		"bin.dat":                  "needle\n\x00\x01",
		"invalid.txt":              "needle\xff\n",
		"frontend/.gitignore":      "out/\n",
		"frontend/out/index.html":  "needle\n",
		"frontend/src/page.ts":     "needle()\n",
	} {
		parent := filepath.Join(dir, filepath.Dir(rel))
		if err := os.MkdirAll(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, parent, filepath.Base(rel), content)
	}
	search := func(t *testing.T, args map[string]any) map[string]any {
		t.Helper()
		a := newTestAgent(t)
		registerFakeFileGrep(a, dir, new(atomic.Int64))
		WrapGrep(a, dir)
		out, err := a.Tools.Execute(context.Background(), "file_grep", args)
		if err != nil {
			t.Fatal(err)
		}
		return out.(map[string]any)
	}
	for _, args := range []map[string]any{
		{"path": ".", "pattern": "needle"},
		{"path": ".", "pattern": "NEEDLE", "case_insensitive": true},
		{"path": ".", "pattern": "need[a-z]+", "regex": true},
		{"path": ".", "pattern": "needle", "glob": "**/*.{ts,tsx}"},
		{"path": ".", "pattern": "needle", "glob": "*.py"},
		{"path": "frontend", "pattern": "needle"},
	} {
		withRG := search(t, args)
		path := os.Getenv("PATH")
		os.Setenv("PATH", "")
		withGo := search(t, args)
		os.Setenv("PATH", path)
		gotRG, gotGo := relFiles(t, dir, withRG), relFiles(t, dir, withGo)
		if !reflect.DeepEqual(gotRG, gotGo) {
			t.Errorf("%v: rg files %v, Go walker files %v", args, gotRG, gotGo)
		}
	}
	got := relFiles(t, dir, search(t, map[string]any{"path": ".", "pattern": "needle"}))
	want := []string{".github/workflows/ci.yml", "frontend/src/page.ts", "src/app.py", "src/view.tsx"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
}

func relFiles(t *testing.T, dir string, res map[string]any) []string {
	t.Helper()
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range res["files"].([]string) {
		if resolved, err := filepath.EvalSymlinks(f); err == nil {
			f = resolved
		}
		rel, err := filepath.Rel(root, f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return sortedStrings(out)
}

func sortedStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

// Searches must not depend on the absolute location of the workspace, honor
// root-relative globs, return a truncated (not failed) result for an
// oversized line, and search an ecosystem directory the caller targets.
func TestGrepRipgrepPathsAndOversizedLines(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := filepath.Join(t.TempDir(), "target", "proj") // ancestor named like an ecosystem dir
	for rel, content := range map[string]string{
		"src/a/x.go":      "needle\n",
		"lib/y.go":        "needle\n",
		"vendor/pkg/v.go": "needle\n",
		"web/min.js":      strings.Repeat("a", 700<<10) + "needle\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Dir(p), filepath.Base(p), content)
	}
	run := func(args map[string]any) (map[string]any, error) {
		a := newTestAgent(t)
		registerFakeFileGrep(a, dir, new(atomic.Int64))
		WrapGrep(a, dir)
		out, err := a.Tools.Execute(context.Background(), "file_grep", args)
		if err != nil {
			return nil, err
		}
		return out.(map[string]any), nil
	}
	tests := []struct {
		args      map[string]any
		want      []string
		truncated bool
	}{
		{map[string]any{"path": ".", "pattern": "needle", "glob": "src/**/*.go"}, []string{"src/a/x.go"}, false},
		{map[string]any{"path": "vendor", "pattern": "needle"}, []string{"pkg/v.go"}, false},
		{map[string]any{"path": "web", "pattern": "needle", "output_mode": "content"}, nil, true},
	}
	for _, tt := range tests {
		res, err := run(tt.args)
		if err != nil {
			t.Fatalf("%v: %v", tt.args, err)
		}
		if tt.truncated {
			if res["truncated"] != true {
				t.Errorf("%v: oversized line not reported truncated: %v", tt.args, res["truncated"])
			}
			continue
		}
		base := dir
		if p, _ := tt.args["path"].(string); p != "." {
			base = filepath.Join(dir, p)
		}
		var got []string
		for _, f := range res["files"].([]string) {
			rel, _ := filepath.Rel(base, f)
			got = append(got, filepath.ToSlash(rel))
		}
		if !reflect.DeepEqual(sortedStrings(got), tt.want) {
			t.Errorf("%v: files %v, want %v", tt.args, got, tt.want)
		}
	}
	res, err := run(map[string]any{"path": ".", "pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	// x.go and y.go; vendor is skipped from the root, and min.js's oversized
	// match event makes the result truncated rather than an error.
	if n := len(res["files"].([]string)); n < 2 || res["truncated"] != true && n != 3 {
		t.Errorf("workspace under a 'target' ancestor returned %d files (truncated=%v): %v", n, res["truncated"], res["files"])
	}
}
