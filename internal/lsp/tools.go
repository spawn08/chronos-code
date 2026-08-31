//go:build lsp

package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spawn08/chronos/engine/tool"
)

// Tools returns the four LSP-backed T0 tools: lsp_diagnostics, lsp_hover,
// lsp_references, and lsp_rename_preview.
func Tools(client *Client, root string) []*tool.Definition {
	if client == nil {
		return nil
	}
	return []*tool.Definition{
		diagnosticsTool(client, root),
		hoverTool(client, root),
		referencesTool(client, root),
		renamePreviewTool(client, root),
	}
}

func resolveURI(root, file string) string {
	if !filepath.IsAbs(file) {
		file = filepath.Join(root, file)
	}
	return FileURI(file)
}

func ensureOpen(ctx context.Context, client *Client, root, file string) (string, error) {
	uri := resolveURI(root, file)
	abs := file
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, abs)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return uri, fmt.Errorf("read %s: %w", file, err)
	}
	lang := detectLanguage(abs)
	_ = client.DidOpen(ctx, uri, lang, string(data))
	return uri, nil
}

func detectLanguage(path string) string {
	switch filepath.Ext(path) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".kt":
		return "kotlin"
	default:
		return "plaintext"
	}
}

func diagnosticsTool(client *Client, root string) *tool.Definition {
	return &tool.Definition{
		Name:        "lsp_diagnostics",
		Description: "Get compiler errors and warnings for a file from the language server, without running a build.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file": map[string]any{"type": "string", "description": "File path (relative to project root or absolute)"},
			},
			"required": []string{"file"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			file, _ := args["file"].(string)
			if file == "" {
				return nil, fmt.Errorf("lsp_diagnostics: file is required")
			}
			uri, err := ensureOpen(ctx, client, root, file)
			if err != nil {
				return nil, err
			}
			diags, err := client.Diagnostics(ctx, uri)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(diags))
			for _, d := range diags {
				out = append(out, map[string]any{
					"severity": SeverityString(d.Severity),
					"message":  d.Message,
					"line":     d.Range.Start.Line + 1,
					"col":      d.Range.Start.Character + 1,
					"source":   d.Source,
				})
			}
			return map[string]any{"file": file, "diagnostics": out, "count": len(out)}, nil
		},
	}
}

func hoverTool(client *Client, root string) *tool.Definition {
	return &tool.Definition{
		Name:        "lsp_hover",
		Description: "Get type information and documentation for a symbol at a position from the language server.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file":      map[string]any{"type": "string", "description": "File path"},
				"line":      map[string]any{"type": "integer", "description": "1-based line number"},
				"character": map[string]any{"type": "integer", "description": "1-based column number"},
			},
			"required": []string{"file", "line", "character"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			file, _ := args["file"].(string)
			line := intArg(args["line"], 0) - 1
			col := intArg(args["character"], 0) - 1
			if file == "" {
				return nil, fmt.Errorf("lsp_hover: file is required")
			}
			uri, err := ensureOpen(ctx, client, root, file)
			if err != nil {
				return nil, err
			}
			result, err := client.Hover(ctx, uri, line, col)
			if err != nil {
				return nil, err
			}
			if result == nil {
				return map[string]any{"found": false}, nil
			}
			return map[string]any{"found": true, "contents": result.Contents}, nil
		},
	}
}

func referencesTool(client *Client, root string) *tool.Definition {
	return &tool.Definition{
		Name:        "lsp_references",
		Description: "Find all references to a symbol at a position — type-aware, no false positives from same-name symbols.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file":      map[string]any{"type": "string", "description": "File path"},
				"line":      map[string]any{"type": "integer", "description": "1-based line number"},
				"character": map[string]any{"type": "integer", "description": "1-based column number"},
			},
			"required": []string{"file", "line", "character"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			file, _ := args["file"].(string)
			line := intArg(args["line"], 0) - 1
			col := intArg(args["character"], 0) - 1
			if file == "" {
				return nil, fmt.Errorf("lsp_references: file is required")
			}
			uri, err := ensureOpen(ctx, client, root, file)
			if err != nil {
				return nil, err
			}
			_ = uri
			locs, err := client.References(ctx, uri, line, col)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(locs))
			for _, l := range locs {
				out = append(out, map[string]any{
					"file": l.URI,
					"line": l.Range.Start.Line + 1,
					"col":  l.Range.Start.Character + 1,
				})
			}
			return map[string]any{"references": out, "count": len(out)}, nil
		},
	}
}

func renamePreviewTool(client *Client, root string) *tool.Definition {
	return &tool.Definition{
		Name:        "lsp_rename_preview",
		Description: "Preview a rename across all files — see what would change, without applying it.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file":      map[string]any{"type": "string", "description": "File path"},
				"line":      map[string]any{"type": "integer", "description": "1-based line number"},
				"character": map[string]any{"type": "integer", "description": "1-based column number"},
				"new_name":  map[string]any{"type": "string", "description": "The new name for the symbol"},
			},
			"required": []string{"file", "line", "character", "new_name"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			file, _ := args["file"].(string)
			line := intArg(args["line"], 0) - 1
			col := intArg(args["character"], 0) - 1
			newName, _ := args["new_name"].(string)
			if file == "" || newName == "" {
				return nil, fmt.Errorf("lsp_rename_preview: file and new_name are required")
			}
			uri, err := ensureOpen(ctx, client, root, file)
			if err != nil {
				return nil, err
			}
			_ = uri
			edit, err := client.RenamePreview(ctx, uri, line, col, newName)
			if err != nil {
				return nil, err
			}
			if edit == nil {
				return map[string]any{"changes": nil}, nil
			}
			fileCount := len(edit.Changes)
			editCount := 0
			for _, edits := range edit.Changes {
				editCount += len(edits)
			}
			return map[string]any{
				"changes":    edit.Changes,
				"file_count": fileCount,
				"edit_count": editCount,
			}, nil
		},
	}
}

func intArg(v any, def int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return def
	}
}
