package projectdocs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchFiresOnCandidateFileChange(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "v1")

	changed := make(chan struct{}, 1)
	w, err := Watch(context.Background(), []string{root}, func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("onChange was not called within 3s of an AGENTS.md write")
	}
}

func TestWatchIgnoresUnrelatedFiles(t *testing.T) {
	root := t.TempDir()

	changed := make(chan struct{}, 1)
	w, err := Watch(context.Background(), []string{root}, func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	if err := os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("noise"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-changed:
		t.Fatal("onChange fired for a non-candidate file")
	case <-time.After(500 * time.Millisecond):
		// expected: no callback
	}
}
