package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestDetect_GoProjectFallbackWalk exercises the filepath.WalkDir fallback
// path (the temp dir is not inside any git repository, so `git ls-files`
// fails and Detect falls back) and asserts language detection plus file
// count regardless of which indexing path is actually taken.
func TestDetect_GoProjectFallbackWalk(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/foo\n")
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	writeFile(t, filepath.Join(root, "README.md"), "hello\n")
	writeFile(t, filepath.Join(root, "sub", "helper.go"), "package sub\n")

	info, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	foundGo := false
	for _, l := range info.Languages {
		if l == "Go" {
			foundGo = true
		}
	}
	if !foundGo {
		t.Errorf("Languages = %v, want to contain Go", info.Languages)
	}

	const wantFiles = 4 // go.mod, main.go, README.md, sub/helper.go
	if info.FileCount != wantFiles {
		t.Errorf("FileCount = %d, want %d (files: %v)", info.FileCount, wantFiles, info.Files)
	}
	if len(info.Files) != info.FileCount {
		t.Errorf("len(Files) = %d, want it to equal FileCount = %d", len(info.Files), info.FileCount)
	}
}

func TestDetect_MultiLanguageMonorepo(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), "{}\n")
	writeFile(t, filepath.Join(root, "Cargo.toml"), "[package]\n")

	info, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	want := map[string]bool{"Node": true, "Rust": true}
	got := map[string]bool{}
	for _, l := range info.Languages {
		got[l] = true
	}
	for lang := range want {
		if !got[lang] {
			t.Errorf("Languages = %v, want to contain %s", info.Languages, lang)
		}
	}
	if len(info.Languages) != 2 {
		t.Errorf("Languages = %v, want exactly 2 entries", info.Languages)
	}
}

func TestDetect_EmptyDirGenericBanner(t *testing.T) {
	root := t.TempDir()

	info, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(info.Languages) != 0 {
		t.Errorf("Languages = %v, want empty", info.Languages)
	}

	banner := info.Banner()
	if !strings.Contains(banner, "generic project") {
		t.Errorf("Banner() = %q, want it to contain %q", banner, "generic project")
	}
}

func TestTool_HandlerReturnsWorkspaceSummary(t *testing.T) {
	info := &Info{
		Root:      "/some/root",
		Languages: []string{"Go", "Node"},
		FileCount: 42,
		Files:     []string{"a.go", "b.js"},
	}

	def := Tool(info)
	if def.Name != "workspace_info" {
		t.Fatalf("Name = %q, want workspace_info", def.Name)
	}
	if def.Permission != "allow" {
		t.Fatalf("Permission = %q, want allow", def.Permission)
	}
	if def.Handler == nil {
		t.Fatal("Handler is nil")
	}

	result, err := def.Handler(context.Background(), nil)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is %T, want map[string]any", result)
	}
	if m["root"] != info.Root {
		t.Errorf("root = %v, want %v", m["root"], info.Root)
	}
	if m["file_count"] != info.FileCount {
		t.Errorf("file_count = %v, want %v", m["file_count"], info.FileCount)
	}
	langs, ok := m["languages"].([]string)
	if !ok {
		t.Fatalf("languages is %T, want []string", m["languages"])
	}
	if len(langs) != 2 || langs[0] != "Go" || langs[1] != "Node" {
		t.Errorf("languages = %v, want [Go Node]", langs)
	}
	if _, hasFiles := m["files"]; hasFiles {
		t.Error("result unexpectedly contains a full files list")
	}
}

// TestDetect_GitRespectsGitignore confirms .gitignore-awareness: with a real
// git repo, a gitignored file must not appear in Files while a tracked file
// does.
func TestDetect_GitRespectsGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	runGit("init")
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/foo\n")
	writeFile(t, filepath.Join(root, "tracked.go"), "package main\n")
	writeFile(t, filepath.Join(root, ".gitignore"), "ignored.txt\n")
	writeFile(t, filepath.Join(root, "ignored.txt"), "should not appear\n")

	runGit("add", "go.mod", "tracked.go", ".gitignore")

	info, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	sort.Strings(info.Files)
	for _, f := range info.Files {
		if f == "ignored.txt" {
			t.Errorf("Files = %v, ignored.txt should be excluded (gitignored + untracked)", info.Files)
		}
	}

	foundTracked := false
	for _, f := range info.Files {
		if f == "tracked.go" {
			foundTracked = true
		}
	}
	if !foundTracked {
		t.Errorf("Files = %v, want to contain tracked.go", info.Files)
	}
}
