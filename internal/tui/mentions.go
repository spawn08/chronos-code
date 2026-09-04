package tui

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	maxAttachedFiles = 8
	maxAttachBytes   = 64 << 10
)

func mentionTokens(message string) []string {
	var out []string
	for _, field := range strings.Fields(message) {
		if !strings.HasPrefix(field, "@") || len(field) < 2 {
			continue
		}
		token := strings.TrimRightFunc(field[1:], func(r rune) bool {
			return strings.ContainsRune(".,;:)]}", r)
		})
		if token != "" {
			out = append(out, token)
		}
	}
	return out
}

func knownAgent(id string, agents []string) bool {
	for _, agent := range agents {
		if agent == id {
			return true
		}
	}
	return false
}

func attachReferencedFiles(root, message string, agents []string) string {
	if root == "" || message == "" {
		return message
	}
	agentSet := make(map[string]struct{}, len(agents))
	for _, id := range agents {
		agentSet[id] = struct{}{}
	}
	var b strings.Builder
	b.WriteString(message)
	seen := make(map[string]struct{})
	attached := 0
	for _, token := range mentionTokens(message) {
		if _, isAgent := agentSet[token]; isAgent {
			continue
		}
		rel, data, err := readWorkspaceFile(root, token)
		if err != nil {
			continue
		}
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}
		attached++
		if attached > maxAttachedFiles {
			break
		}
		b.WriteString("\n\n<file path=\"")
		b.WriteString(rel)
		b.WriteString("\">\n")
		b.Write(data)
		if !bytes.HasSuffix(data, []byte("\n")) {
			b.WriteByte('\n')
		}
		b.WriteString("</file>")
	}
	return b.String()
}

func readWorkspaceFile(root, token string) (string, []byte, error) {
	if root == "" || token == "" || strings.ContainsRune(token, 0) {
		return "", nil, fmt.Errorf("invalid file reference")
	}
	if filepath.IsAbs(token) {
		return "", nil, fmt.Errorf("absolute path")
	}
	cleaned := filepath.Clean(token)
	if cleaned == "." || strings.HasPrefix(cleaned, "..") {
		return "", nil, fmt.Errorf("path escape")
	}
	full := filepath.Join(root, cleaned)
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", nil, fmt.Errorf("path escape")
	}
	info, err := os.Stat(full)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("not a file")
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", nil, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", nil, fmt.Errorf("binary file")
	}
	if len(data) > maxAttachBytes {
		data = append(append([]byte(nil), data[:maxAttachBytes]...), []byte("\n... [truncated]")...)
	}
	return filepath.ToSlash(rel), data, nil
}

func completionSpan(input string) (prefix, query string) {
	start := 0
	for i, r := range input {
		if unicode.IsSpace(r) {
			start = i + len(string(r))
		}
	}
	if start < len(input) && input[start] == '@' {
		return input[:start], input[start:]
	}
	return "", input
}

func applyCompletion(input, completion string) string {
	prefix, _ := completionSpan(input)
	return prefix + completion
}
