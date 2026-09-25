package incctx

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentScansOwnRetainedContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	// Cross several buffer refills and then reuse pooled readers concurrently.
	data := "needle first\n" + strings.Repeat("other line\n", 20000) + "needle last"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			search := grepSearch{remaining: grepMaxScanBytes, matcher: func(line []byte) bool {
				return bytes.Contains(line, []byte("needle"))
			}}
			if err := search.file(context.Background(), path, false); err != nil {
				t.Error(err)
				return
			}
			if _, err := readRange(context.Background(), path, 2, 10); err != nil {
				t.Error(err)
			}
			if len(search.matches) != 2 || search.matches[0]["content"] != "needle first" || search.matches[1]["content"] != "needle last" {
				t.Errorf("retained matches changed after buffer reuse: %v", search.matches)
			}
		})
	}
	wg.Wait()
}

func BenchmarkFileOperations(b *testing.B) {
	root := b.TempDir()
	path := filepath.Join(root, "source.txt")
	data := strings.Repeat("ordinary source line with no matching identifier\n", 100000)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		b.Fatal(err)
	}
	b.Run("SparseSearch", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			search := grepSearch{remaining: grepMaxScanBytes, matcher: func(line []byte) bool {
				return bytes.Contains(line, []byte("needle"))
			}}
			if err := search.file(context.Background(), path, false); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("LateRange", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := readRange(context.Background(), path, 90000, 90010); err != nil {
				b.Fatal(err)
			}
		}
	})
}
