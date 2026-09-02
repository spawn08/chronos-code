package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// Note: lipgloss auto-detects color-profile support and disables styling
// entirely when stdout isn't a terminal (true for `go test`), so under test
// RenderMarkdownLite's output is plain text — these assertions check content
// and marker-stripping, not ANSI escape codes.

func TestRenderMarkdownLite_Header(t *testing.T) {
	got := RenderMarkdownLite("# Title", 0)
	if got != "Title" {
		t.Errorf("RenderMarkdownLite(header) = %q, want %q", got, "Title")
	}
	got = RenderMarkdownLite("### Subheading", 0)
	if got != "Subheading" {
		t.Errorf("RenderMarkdownLite(h3) = %q, want %q", got, "Subheading")
	}
}

func TestRenderMarkdownLite_BoldItalicCode(t *testing.T) {
	got := RenderMarkdownLite("this is **bold** and _italic_ and `code`", 0)
	want := "this is bold and italic and code"
	if got != want {
		t.Errorf("RenderMarkdownLite(inline) = %q, want %q", got, want)
	}
}

func TestRenderMarkdownLite_BulletList(t *testing.T) {
	got := RenderMarkdownLite("- first\n- second", 0)
	want := "• first\n• second"
	if got != want {
		t.Errorf("RenderMarkdownLite(bullets) = %q, want %q", got, want)
	}
}

func TestRenderMarkdownLite_NumberedList(t *testing.T) {
	got := RenderMarkdownLite("1. one\n2. two", 0)
	want := "1. one\n2. two"
	if got != want {
		t.Errorf("RenderMarkdownLite(numbered) = %q, want %q", got, want)
	}
}

func TestRenderMarkdownLite_Blockquote(t *testing.T) {
	got := RenderMarkdownLite("> quoted text", 0)
	want := "│ quoted text"
	if got != want {
		t.Errorf("RenderMarkdownLite(blockquote) = %q, want %q", got, want)
	}
}

func TestRenderMarkdownLite_FencedCodeBlockVerbatim(t *testing.T) {
	src := "before\n```go\nfunc f() **not bold** { }\n```\nafter"
	got := RenderMarkdownLite(src, 0)
	if !contains(got, "func f() **not bold** { }") {
		t.Errorf("RenderMarkdownLite(fenced) = %q, code content must be preserved verbatim (no inline markdown processing)", got)
	}
	if contains(got, "```") {
		t.Errorf("RenderMarkdownLite(fenced) = %q, fence delimiters should not appear in output", got)
	}
	if !contains(got, "go") {
		t.Errorf("RenderMarkdownLite(fenced) = %q, want the fence's language tag %q rendered", got, "go")
	}
	if !contains(got, "before") || !contains(got, "after") {
		t.Errorf("RenderMarkdownLite(fenced) = %q, surrounding text must be preserved", got)
	}
}

func TestRenderMarkdownLite_Empty(t *testing.T) {
	if got := RenderMarkdownLite("", 0); got != "" {
		t.Errorf("RenderMarkdownLite(\"\") = %q, want empty", got)
	}
}

func TestSummarizeArgs(t *testing.T) {
	tests := []struct {
		name, args, want string
	}{
		{"short", `{"a":1}`, `{"a":1}`},
		{"newlines flattened", "line1\nline2\nline3", "line1 line2 line3"},
		{"truncated", string(make([]byte, 100)), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "truncated" {
				got := SummarizeArgs(string(make([]byte, 100)))
				if len(got) != 80 {
					t.Errorf("len(SummarizeArgs(100 bytes)) = %d, want 80", len(got))
				}
				return
			}
			if got := SummarizeArgs(tt.args); got != tt.want {
				t.Errorf("SummarizeArgs(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestFormatArgsExcept(t *testing.T) {
	args := map[string]any{"command": "ls -la", "working_dir": "/tmp", "timeout_sec": 30}
	got := FormatArgsExcept(args, "command", "working_dir")
	want := "timeout_sec=30"
	if got != want {
		t.Errorf("FormatArgsExcept() = %q, want %q", got, want)
	}
}

func TestRenderFileWriteDiff_NewFile(t *testing.T) {
	args := map[string]any{"path": "foo.go", "new_content": "package foo\n", "create": true}
	got := RenderFileWriteDiff(args)
	if got == "" {
		t.Fatal("RenderFileWriteDiff() = \"\", want non-empty")
	}
	if !contains(got, "foo.go") || !contains(got, "package foo") || !contains(got, "new file") {
		t.Errorf("RenderFileWriteDiff(new file) = %q, missing expected content", got)
	}
}

func TestRenderFileWriteDiff_Replace(t *testing.T) {
	args := map[string]any{"path": "foo.go", "old_content": "old\n", "new_content": "new\n"}
	got := RenderFileWriteDiff(args)
	if !contains(got, "- old") || !contains(got, "+ new") {
		t.Errorf("RenderFileWriteDiff(replace) = %q, want lines containing '- old' and '+ new'", got)
	}
}

// TestRenderMarkdownLite_NeverExceedsWidth guards against the class of bug
// that made the status bar overflow its budget and wrap onto a second line,
// corrupting the fixed-height layout around it (see styles.go's comment on
// styleHeaderBar): every rendered line — headers, blockquotes, bullets,
// plain paragraphs, and code blocks — must fit within width, since the
// interactive REPL's viewport never wraps long lines itself.
func TestRenderMarkdownLite_NeverExceedsWidth(t *testing.T) {
	const width = 20
	src := strings.Join([]string{
		"# A heading that is much longer than the available width",
		"",
		"A plain paragraph sentence that also runs well past the configured width limit.",
		"",
		"> A blockquote line that likewise exceeds the width budget by a wide margin.",
		"",
		"- A bullet point whose text is longer than the width so it must wrap or truncate",
		"",
		"```go",
		"aVeryLongUnbrokenIdentifierNameThatCannotBeWrapped := 1",
		"```",
	}, "\n")

	got := RenderMarkdownLite(src, width)
	for i, line := range strings.Split(got, "\n") {
		if w := lipgloss.Width(line); w > width {
			t.Errorf("line %d exceeds width %d (got %d): %q", i, width, w, line)
		}
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
