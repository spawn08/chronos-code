package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestAttachmentAggregateBudgetAndReceipts(t *testing.T) {
	root := t.TempDir()
	message := "Keep this instruction verbatim."
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("source%d.txt", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte(strings.Repeat("世界\n", 20000)), 0o600); err != nil {
			t.Fatal(err)
		}
		message += " @" + name
	}
	input, err := prepareInput(context.Background(), root, message, nil, maxInputBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Message) > maxInputBytes || !utf8.ValidString(input.Message) || !strings.HasPrefix(input.Message, message) {
		t.Fatalf("invalid aggregate: %d bytes", len(input.Message))
	}
	for i := 0; i < 10; i++ {
		status := "truncated:"
		if i >= 8 {
			status = "omitted: 8-file limit"
		}
		if !strings.Contains(input.Receipt, fmt.Sprintf("\"source%d.txt\": %s", i, status)) {
			t.Fatalf("missing receipt for source %d: %s", i, input.Receipt)
		}
	}
	if strings.Count(input.Message, "<file path=") != 8 || !strings.Contains(input.Message, "file_read") {
		t.Fatalf("missing excerpts/retrieval hint: %s", input.Receipt)
	}
	if got := attachReferencedFiles(root, message, nil); len(got)-len(message) > maxInputBytes {
		t.Fatalf("legacy helper appended %d bytes", len(got)-len(message))
	}
}

func TestAttachmentEmptyMissingBinaryEscapesAndDuplicate(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"empty": "", "binary": "abc\x00def", "ok": "hello"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	input, err := prepareInput(context.Background(), root, "inspect @empty @missing @binary @../escape @link @ok @./ok @coder", []string{"coder"}, maxInputBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"empty": empty (0 bytes)`, `"missing": error: missing file`, `"binary": error: binary file`, `"../escape": error: path escape`, `"link": error: path escape`, `"ok": included: 5 bytes`} {
		if !strings.Contains(input.Receipt, want) {
			t.Errorf("missing %s in %s", want, input.Receipt)
		}
	}
	if strings.Count(input.Receipt, `"ok":`) != 1 || strings.Contains(input.Message, "outside-secret") || strings.Contains(input.Receipt, "coder") {
		t.Fatalf("invalid receipt: %s", input.Receipt)
	}
}

func TestWorkspaceExcerptBoundsSparseFile(t *testing.T) {
	root := t.TempDir()
	f, err := os.Create(filepath.Join(root, "huge"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat("a", maxAttachBytes+1)); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, data, size, err := readWorkspaceExcerpt(context.Background(), root, "huge", maxAttachBytes)
	if err != nil || len(data) != maxAttachBytes || size != 1<<30 {
		t.Fatalf("bounded sparse read: bytes=%d size=%d err=%v", len(data), size, err)
	}
}

func TestAttachmentReceiptOverflowIsRetrievable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHRONOS_CODE_DATA_HOME", t.TempDir())
	var refs []string
	for i := 0; i < 100; i++ {
		refs = append(refs, fmt.Sprintf("@file%d", i))
	}
	input, err := prepareInput(context.Background(), root, strings.Join(refs, " "), nil, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Message) > 2048 || !strings.Contains(input.Message, "Full per-file receipt:") {
		t.Fatalf("overflow receipt missing: %s", input.Message)
	}
	paths, _ := filepath.Glob(filepath.Join(os.Getenv("CHRONOS_CODE_DATA_HOME"), "projects", "*", "artifacts", "attachments-*.txt"))
	if len(paths) != 1 {
		t.Fatalf("receipt artifacts: %v", paths)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil || strings.Count(string(data), "omitted:") != 100 {
		t.Fatalf("incomplete receipt artifact: %v\n%s", err, data)
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
