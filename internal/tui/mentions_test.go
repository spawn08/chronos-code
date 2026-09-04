package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachReferencedFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "internal", "tui")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "app.go"), []byte("package tui\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := attachReferencedFiles(root, "please review @internal/tui/app.go", []string{"coder"})
	if !strings.Contains(got, "please review @internal/tui/app.go") {
		t.Fatalf("original mention missing: %q", got)
	}
	if !strings.Contains(got, `<file path="internal/tui/app.go">`) || !strings.Contains(got, "package tui") {
		t.Fatalf("file contents were not attached: %q", got)
	}

	escaped := attachReferencedFiles(root, "ignore @../etc/passwd", nil)
	if strings.Contains(escaped, "<file") {
		t.Fatalf("escaped path was attached: %q", escaped)
	}

	agentOnly := attachReferencedFiles(root, "@coder look at this", []string{"coder"})
	if strings.Contains(agentOnly, "<file") {
		t.Fatalf("agent mention attached as file: %q", agentOnly)
	}
}

func TestApplyCompletionReplacesAtToken(t *testing.T) {
	got := applyCompletion("look at @app", "@internal/tui/app.go")
	if got != "look at @internal/tui/app.go" {
		t.Fatalf("applyCompletion() = %q", got)
	}
	if got := applyCompletion("/ag", "/agent"); got != "/agent" {
		t.Fatalf("slash applyCompletion() = %q", got)
	}
}
