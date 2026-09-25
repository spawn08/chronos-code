// Package claims implements self-invalidating working memory for long-horizon
// agent runs. A claim is a belief the agent holds about the code ("Parse
// rejects empty input", "retry budget lives in budget.go"). Every claim is
// anchored to exact content-hashed spans of workspace files and, for symbol
// anchors, to the spans of the symbols it calls. When anchored content
// changes the harness marks the claim stale deterministically, cascades the
// doubt to claims derived from it, and restores it if the content returns.
//
// Unlike conversation summaries, claims know when they have become wrong.
package claims

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Anchor pins a claim to a span of one workspace file. StartLine and EndLine
// are 1-based and inclusive; a zero StartLine anchors the whole file.
type Anchor struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Symbol    string `json:"symbol,omitempty"`
	Hash      string `json:"hash"`
}

// String renders the anchor as path[:start-end][ symbol].
func (a Anchor) String() string {
	var b strings.Builder
	b.WriteString(a.Path)
	if a.StartLine > 0 {
		fmt.Fprintf(&b, ":%d-%d", a.StartLine, a.EndLine)
	}
	if a.Symbol != "" {
		b.WriteString(" ")
		b.WriteString(a.Symbol)
	}
	return b.String()
}

func (a Anchor) wholeFile() bool { return a.StartLine == 0 }

// normalizePath converts path to a clean, slash-separated path relative to
// root and rejects paths that escape it.
func normalizePath(root, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("anchor path is required")
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, abs)
	}
	rel, err := filepath.Rel(root, filepath.Clean(abs))
	if err != nil {
		return "", fmt.Errorf("anchor path %q: %w", path, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("anchor path %q is outside the workspace", path)
	}
	return filepath.ToSlash(rel), nil
}

// fileLines reads a workspace file. ok is false when it cannot be read.
func fileLines(root, rel string) (lines []string, ok bool) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, false
	}
	return strings.Split(string(data), "\n"), true
}

func hashSpan(lines []string, start, end int) string {
	h := sha256.New()
	for i := start - 1; i < end; i++ {
		h.Write([]byte(lines[i]))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashAnchor computes the hash of an anchor's span in lines.
func hashAnchor(lines []string, a Anchor) (string, error) {
	if a.wholeFile() {
		return hashSpan(lines, 1, len(lines)), nil
	}
	if a.StartLine < 1 || a.EndLine < a.StartLine || a.EndLine > len(lines) {
		return "", fmt.Errorf("anchor %s: line range outside file (1-%d)", a, len(lines))
	}
	return hashSpan(lines, a.StartLine, a.EndLine), nil
}

// locate finds the anchor's content in lines. A span whose text moved (lines
// inserted or removed above it) is relocated to the nearest identical window
// rather than reported as changed. ok is false when the content is gone.
func locate(lines []string, a Anchor) (Anchor, bool) {
	if lines == nil {
		return a, false
	}
	if a.wholeFile() {
		return a, hashSpan(lines, 1, len(lines)) == a.Hash
	}
	if a.EndLine <= len(lines) && a.StartLine >= 1 && hashSpan(lines, a.StartLine, a.EndLine) == a.Hash {
		return a, true
	}
	width := a.EndLine - a.StartLine + 1
	best, bestDistance := 0, -1
	for start := 1; start+width-1 <= len(lines); start++ {
		if hashSpan(lines, start, start+width-1) != a.Hash {
			continue
		}
		distance := start - a.StartLine
		if distance < 0 {
			distance = -distance
		}
		if bestDistance < 0 || distance < bestDistance {
			best, bestDistance = start, distance
		}
	}
	if bestDistance < 0 {
		return a, false
	}
	a.StartLine, a.EndLine = best, best+width-1
	return a, true
}
