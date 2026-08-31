//go:build lsp

package lsp

// Diagnostic represents a compiler error, warning, or informational message.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"` // 1=Error, 2=Warning, 3=Info, 4=Hint
	Message  string `json:"message"`
	Source   string `json:"source"`
}

// HoverResult holds the response from textDocument/hover.
type HoverResult struct {
	Contents any    `json:"contents"`
	Range    *Range `json:"range,omitempty"`
}

// Location is a file position returned by references/definition.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Range is a start–end pair of positions within a file.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Position is a zero-based line and character offset.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// WorkspaceEdit describes a set of text edits across files.
type WorkspaceEdit struct {
	Changes map[string][]TextEdit `json:"changes"`
}

// TextEdit is a replacement of a range with new text.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// SeverityString returns a human-readable label for a diagnostic severity.
func SeverityString(severity int) string {
	switch severity {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	default:
		return "unknown"
	}
}
