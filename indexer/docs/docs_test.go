package docs

import (
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

func TestKind(t *testing.T) {
	for rel, want := range map[string]string{
		"README.md": LangMarkdown, "docs/a.mdx": LangMarkdown, "notes.txt": LangText, "docs/x.rst": LangText,
		"CMakeLists.txt": "", "requirements-dev.txt": "", "LICENSE.txt": "", "main.go": "",
	} {
		if got := Kind(rel); got != want {
			t.Errorf("Kind(%q) = %q, want %q", rel, got, want)
		}
	}
}

func TestCodeShaped(t *testing.T) {
	for w, want := range map[string]bool{
		"parseFile": true, "HTTPServer": false, "max_tokens": true, "MAX_TOKENS": true, "Engine.Update": true, "os.Exit": true,
		"README.md": false, "e.g": false, "Hello": false, "hello": false, "__init__": false, "iOS": false,
	} {
		if got := codeShaped(w); got != want {
			t.Errorf("codeShaped(%q) = %v, want %v", w, got, want)
		}
	}
}

// TestTextBlocks splits a long text file at blank lines into blocks named
// after the file and their first line.
func TestTextBlocks(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		if i == 70 {
			b.WriteString("\nSecond part begins\n")
		}
		b.WriteString("line of text\n")
	}
	f := &facts.File{Path: "notes/log.txt"}
	Extract(f, LangText, []byte(b.String()))
	if len(f.Symbols) != 2 || f.Symbols[0].Name != "log.txt" || f.Symbols[1].Name != "log.txt: Second part begins" {
		var names []string
		for _, s := range f.Symbols {
			names = append(names, s.Name)
		}
		t.Fatalf("blocks = %q", names)
	}
}
