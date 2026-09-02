package security

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCaptureRetainsLast500Lines(t *testing.T) {
	w := NewCaptureWriter()
	for i := 1; i <= 2000; i++ {
		if _, err := fmt.Fprintf(w, "line-%04d\n", i); err != nil {
			t.Fatalf("write line %d: %v", i, err)
		}
	}

	got := w.Snapshot()
	if got.TotalLines != 2000 || got.OmittedLines != 1500 || !got.Truncated {
		t.Fatalf("counts = total %d, omitted %d, truncated %v", got.TotalLines, got.OmittedLines, got.Truncated)
	}
	if len(got.Lines) != CaptureLineLimit {
		t.Fatalf("retained %d lines, want %d", len(got.Lines), CaptureLineLimit)
	}
	for i, line := range got.Lines {
		want := fmt.Sprintf("line-%04d", i+1501)
		if line != want {
			t.Fatalf("line %d = %q, want %q", i, line, want)
		}
	}
}

func TestCapturePartialWritesAndFinalUnterminatedLine(t *testing.T) {
	w := NewCaptureWriter()
	for _, part := range []string{"fir", "st\nsecond\nthi", "rd"} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatalf("Write(%q): %v", part, err)
		}
	}

	got := w.Snapshot()
	want := []string{"first", "second", "third"}
	assertCaptureLines(t, got.Lines, want)
	if got.TotalLines != 3 || got.OmittedLines != 0 || got.Truncated {
		t.Fatalf("counts = total %d, omitted %d, truncated %v", got.TotalLines, got.OmittedLines, got.Truncated)
	}

	if _, err := w.Write([]byte("-continued\n")); err != nil {
		t.Fatalf("continue final line: %v", err)
	}
	assertCaptureLines(t, w.Snapshot().Lines, []string{"first", "second", "third-continued"})
}

func TestCaptureHugeLineIsBounded(t *testing.T) {
	w := NewCaptureWriter()
	huge := strings.Repeat("x", CaptureLineByteLimit*3) + "tail"
	if _, err := w.Write([]byte(huge)); err != nil {
		t.Fatalf("write huge line: %v", err)
	}

	got := w.Snapshot()
	if len(got.Lines) != 1 || len(got.Lines[0]) != CaptureLineByteLimit {
		t.Fatalf("retained line sizes = %v, want one %d-byte line", lineLengths(got.Lines), CaptureLineByteLimit)
	}
	if !strings.HasSuffix(got.Lines[0], "tail") || !got.Truncated {
		t.Fatalf("huge line suffix/truncation not preserved")
	}
}

func TestBoundedSummaryUsesDeterministicSuffix(t *testing.T) {
	output := CapturedOutput{Lines: []string{"aaaa", "bbbb", "cccc"}, TotalLines: 3}
	const want = "bbb\ncccc"
	if got := Summarize(output, 2); got != want {
		t.Fatalf("Summarize = %q, want %q", got, want)
	}
	if got := Summarize(output, 2); got != want {
		t.Fatalf("second Summarize = %q, want %q", got, want)
	}
	for _, budget := range []int{0, -1} {
		if got := Summarize(output, budget); got != "" {
			t.Fatalf("Summarize budget %d = %q, want empty", budget, got)
		}
	}
}

func TestCaptureConcurrentWritesAndSnapshots(t *testing.T) {
	w := NewCaptureWriter()
	const writers = 8
	const linesPerWriter = 300

	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for line := 0; line < linesPerWriter; line++ {
				_, _ = fmt.Fprintf(w, "%d:%d\n", writer, line)
				_ = w.Snapshot()
			}
		}(writer)
	}
	wg.Wait()

	got := w.Snapshot()
	wantTotal := writers * linesPerWriter
	if got.TotalLines != wantTotal || got.OmittedLines != wantTotal-CaptureLineLimit {
		t.Fatalf("counts = total %d, omitted %d; want %d, %d", got.TotalLines, got.OmittedLines, wantTotal, wantTotal-CaptureLineLimit)
	}
	if len(got.Lines) != CaptureLineLimit {
		t.Fatalf("retained %d lines, want %d", len(got.Lines), CaptureLineLimit)
	}
}

func assertCaptureLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func lineLengths(lines []string) []int {
	lengths := make([]int, len(lines))
	for i := range lines {
		lengths[i] = len(lines[i])
	}
	return lengths
}
