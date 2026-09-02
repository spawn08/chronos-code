package security

import (
	"bytes"
	"strings"
	"sync"
)

const (
	// CaptureLineLimit is the number of most recent logical lines retained.
	CaptureLineLimit = 500
	// CaptureLineByteLimit bounds memory use when output contains one huge line.
	CaptureLineByteLimit = 64 * 1024
	// CaptureBytesPerToken is the conservative approximation used by Summarize.
	CaptureBytesPerToken = 4
)

// CapturedOutput is an immutable snapshot of a CaptureWriter.
type CapturedOutput struct {
	Lines        []string
	TotalLines   int
	OmittedLines int
	Truncated    bool
}

// CaptureWriter retains a bounded tail of logical lines. Its zero value is
// ready for use and all methods are safe for concurrent calls.
type CaptureWriter struct {
	mu sync.Mutex

	lines [CaptureLineLimit]string
	start int
	count int
	total int

	partial          []byte
	partialActive    bool
	contentTruncated bool
}

var _ interface{ Write([]byte) (int, error) } = (*CaptureWriter)(nil)

// NewCaptureWriter returns an empty bounded output writer.
func NewCaptureWriter() *CaptureWriter {
	return &CaptureWriter{}
}

// Write implements io.Writer. Each call is atomic relative to other writes.
func (w *CaptureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	written := len(p)
	for len(p) > 0 {
		newline := bytes.IndexByte(p, '\n')
		if newline < 0 {
			w.appendPartial(p)
			w.partialActive = true
			break
		}

		w.appendPartial(p[:newline])
		w.commitPartial()
		p = p[newline+1:]
	}
	return written, nil
}

func (w *CaptureWriter) appendPartial(p []byte) {
	if len(w.partial)+len(p) <= CaptureLineByteLimit {
		w.partial = append(w.partial, p...)
		return
	}

	w.contentTruncated = true
	if len(p) >= CaptureLineByteLimit {
		w.partial = append(w.partial[:0], p[len(p)-CaptureLineByteLimit:]...)
		return
	}

	drop := len(w.partial) + len(p) - CaptureLineByteLimit
	copy(w.partial, w.partial[drop:])
	w.partial = w.partial[:len(w.partial)-drop]
	w.partial = append(w.partial, p...)
}

func (w *CaptureWriter) commitPartial() {
	line := string(w.partial)
	if w.count < CaptureLineLimit {
		index := (w.start + w.count) % CaptureLineLimit
		w.lines[index] = line
		w.count++
	} else {
		w.lines[w.start] = line
		w.start = (w.start + 1) % CaptureLineLimit
	}
	w.total++
	w.partial = w.partial[:0]
	w.partialActive = false
}

// Snapshot returns the retained lines in logical order. An unterminated line
// is included without committing it, so a later write can continue that line.
func (w *CaptureWriter) Snapshot() CapturedOutput {
	w.mu.Lock()
	defer w.mu.Unlock()

	retained := w.count
	if w.partialActive {
		retained++
	}
	skip := 0
	if retained > CaptureLineLimit {
		skip = retained - CaptureLineLimit
		retained = CaptureLineLimit
	}

	lines := make([]string, 0, retained)
	for i := skip; i < w.count; i++ {
		lines = append(lines, w.lines[(w.start+i)%CaptureLineLimit])
	}
	if w.partialActive {
		lines = append(lines, string(w.partial))
	}

	total := w.total
	if w.partialActive {
		total++
	}
	omitted := total - len(lines)
	return CapturedOutput{
		Lines:        lines,
		TotalLines:   total,
		OmittedLines: omitted,
		Truncated:    omitted > 0 || w.contentTruncated,
	}
}

// Summarize renders the largest byte suffix that fits maxTokens using the
// documented CaptureBytesPerToken approximation. Lines are joined with "\n".
func Summarize(output CapturedOutput, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	maxInt := int(^uint(0) >> 1)
	budget := maxInt
	if maxTokens <= maxInt/CaptureBytesPerToken {
		budget = maxTokens * CaptureBytesPerToken
	}

	text := strings.Join(output.Lines, "\n")
	if len(text) <= budget {
		return text
	}
	return text[len(text)-budget:]
}
