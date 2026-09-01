package projectdocs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMergesRootToLeafInOrder(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root instructions")
	sub := filepath.Join(root, "pkg", "sub")
	writeFile(t, filepath.Join(sub, "CLAUDE.md"), "leaf instructions")

	b, err := Load(root, sub)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(b.Docs) != 2 {
		t.Fatalf("len(Docs) = %d, want 2", len(b.Docs))
	}
	if b.Docs[0].Body != "root instructions" {
		t.Errorf("Docs[0].Body = %q, want root instructions (root-first order)", b.Docs[0].Body)
	}
	if b.Docs[1].Body != "leaf instructions" {
		t.Errorf("Docs[1].Body = %q, want leaf instructions (leaf-last order)", b.Docs[1].Body)
	}

	raw := b.raw()
	rootIdx := strings.Index(raw, "root instructions")
	leafIdx := strings.Index(raw, "leaf instructions")
	if rootIdx == -1 || leafIdx == -1 || rootIdx > leafIdx {
		t.Fatalf("raw() = %q, want root instructions before leaf instructions", raw)
	}
}

func TestLoadPicksUpMultipleCandidateNamesAtOneLevel(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "agents body")
	writeFile(t, filepath.Join(root, ".cursorrules"), "cursor body")

	b, err := Load(root, root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(b.Docs) != 2 {
		t.Fatalf("len(Docs) = %d, want 2", len(b.Docs))
	}
}

func TestLoadCopilotInstructionsNestedPath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".github", "copilot-instructions.md"), "copilot body")

	b, err := Load(root, root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(b.Docs) != 1 || b.Docs[0].Body != "copilot body" {
		t.Fatalf("Docs = %+v, want single copilot-instructions.md doc", b.Docs)
	}
}

func TestLoadEmptyWorkspaceReturnsEmptyBundle(t *testing.T) {
	root := t.TempDir()
	b, err := Load(root, root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !b.Empty() {
		t.Fatalf("Empty() = false, want true for a workspace with no instructions files")
	}
	if b.raw() != "" {
		t.Errorf("raw() = %q, want empty", b.raw())
	}
}

func TestLoadRejectsCwdOutsideRoot(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	if _, err := Load(root, other); err == nil {
		t.Fatal("Load: want error when cwd is not root or a descendant of it")
	}
}

func TestRenderUnderBudgetReturnsVerbatim(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "short instructions")
	b, err := Load(root, root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cachePath := filepath.Join(t.TempDir(), "cache.json")
	out, err := Render(context.Background(), b, "gpt-4o", cachePath, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "short instructions") {
		t.Errorf("Render output = %q, want it to contain the raw body verbatim", out)
	}
}

func TestRenderOverBudgetUsesSummarizerAndCaches(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), strings.Repeat("word ", 20000))
	b, err := Load(root, root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cachePath := filepath.Join(t.TempDir(), "cache.json")
	calls := 0
	summarize := func(ctx context.Context, text string) (string, error) {
		calls++
		return "condensed summary", nil
	}

	out, err := Render(context.Background(), b, "gpt-4o", cachePath, summarize)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "condensed summary" {
		t.Fatalf("Render output = %q, want condensed summary", out)
	}
	if calls != 1 {
		t.Fatalf("summarize called %d times, want 1", calls)
	}

	// Second call with identical content must hit the cache, not summarize again.
	out2, err := Render(context.Background(), b, "gpt-4o", cachePath, summarize)
	if err != nil {
		t.Fatalf("Render (cached): %v", err)
	}
	if out2 != "condensed summary" || calls != 1 {
		t.Fatalf("Render (cached) = %q, calls = %d, want cache hit with no extra summarize call", out2, calls)
	}
}

func TestRenderOverBudgetFallsBackToHardTruncateWithoutSummarizer(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), strings.Repeat("word ", 20000))
	b, err := Load(root, root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cachePath := filepath.Join(t.TempDir(), "cache.json")
	out, err := Render(context.Background(), b, "gpt-4o", cachePath, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("Render output missing truncation marker: %q", out[:min(200, len(out))])
	}
}

func TestWatchDirsIncludesGithubSubdirWhenPresent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}

	dirs, err := WatchDirs(root, root)
	if err != nil {
		t.Fatalf("WatchDirs: %v", err)
	}
	found := false
	for _, d := range dirs {
		if d == filepath.Join(root, ".github") {
			found = true
		}
	}
	if !found {
		t.Fatalf("WatchDirs() = %v, want it to include the .github subdirectory", dirs)
	}
}
