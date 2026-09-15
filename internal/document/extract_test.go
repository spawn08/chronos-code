package document

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExtractDOCX(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.docx")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	entry, err := w.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(`<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>First paragraph</w:t></w:r></w:p><w:p><w:r><w:t>Second paragraph</w:t></w:r></w:p></w:body></w:document>`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := Extract(context.Background(), path, 1024)
	if err != nil || got != "First paragraph\nSecond paragraph" {
		t.Fatalf("Extract = %q, %v", got, err)
	}
	if !Supported(path) || Supported("notes.txt") {
		t.Fatal("format detection is incorrect")
	}
}

func TestExtractDOCXBoundsOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.docx")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	entry, err := w.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(`<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>` + strings.Repeat("界", 100) + `</w:t></w:r></w:p></w:body></w:document>`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := Extract(context.Background(), path, 10)
	if err != nil || len(got) > 10 || !utf8.ValidString(got) {
		t.Fatalf("bounded Extract = %q, %v", got, err)
	}
}
