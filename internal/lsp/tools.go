//go:build lsp

package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spawn08/chronos/engine/tool"
)

const (
	diagnosticLimit = 50
	diagnosticTTL   = 5 * time.Minute
)

type openDocument struct {
	text    string
	version int
}

type toolState struct {
	manager *Manager
	root    string
	rootErr error

	mu   sync.Mutex
	open map[string]openDocument
}

// Tools returns the four LSP-backed T0 tools: lsp_diagnostics, lsp_hover,
// lsp_references, and lsp_rename_preview.
func Tools(manager *Manager, root string) []*tool.Definition {
	if manager == nil {
		return nil
	}
	state := newToolState(manager, root)
	return []*tool.Definition{
		diagnosticsTool(state),
		hoverTool(state),
		referencesTool(state),
		renamePreviewTool(state),
	}
}

func newToolState(manager *Manager, root string) *toolState {
	abs, err := filepath.Abs(root)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	return &toolState{manager: manager, root: filepath.Clean(abs), rootErr: err, open: make(map[string]openDocument)}
}

func (s *toolState) canonicalFile(file string) (string, error) {
	if s.rootErr != nil {
		return "", fmt.Errorf("lsp: resolve workspace root: %w", s.rootErr)
	}
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.root, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("lsp: resolve %s: %w", file, err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("lsp: resolve %s: %w", file, err)
	}
	rel, err := filepath.Rel(s.root, canonical)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("lsp: file %q is outside workspace", file)
	}
	return canonical, nil
}

func (s *toolState) clientAndDocument(ctx context.Context, file string) (ManagedClient, string, error) {
	path, err := s.canonicalFile(file)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", file, err)
	}
	client, available, err := s.manager.ClientFor(ctx, path)
	if err != nil {
		return nil, "", err
	}
	if !available {
		return nil, "", fmt.Errorf("lsp: no language server available for %s", file)
	}

	uri := FileURI(path)
	text := string(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	document, opened := s.open[uri]
	if !opened {
		if err := client.DidOpen(ctx, uri, detectLanguage(path), text); err != nil {
			return nil, "", fmt.Errorf("lsp: open %s: %w", file, err)
		}
		s.open[uri] = openDocument{text: text, version: 1}
	} else if document.text != text {
		document.version++
		if err := changeDocument(ctx, client, uri, document.version, text); err != nil {
			return nil, "", fmt.Errorf("lsp: change %s: %w", file, err)
		}
		document.text = text
		s.open[uri] = document
	}
	return client, uri, nil
}

type documentChanger interface {
	DidChange(context.Context, string, int, string) error
}

func changeDocument(ctx context.Context, client ManagedClient, uri string, version int, text string) error {
	if changer, ok := client.(documentChanger); ok {
		return changer.DidChange(ctx, uri, version, text)
	}
	if concrete, ok := client.(*Client); ok {
		return concrete.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": version},
			"contentChanges": []map[string]any{{"text": text}},
		})
	}
	return fmt.Errorf("client does not support document changes")
}

func detectLanguage(path string) string {
	language, _, _, ok := ResolveServer(path)
	if !ok {
		return "plaintext"
	}
	return language
}

func diagnosticsTool(state *toolState) *tool.Definition {
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
			client, uri, err := state.clientAndDocument(ctx, file)
			if err != nil {
				return nil, err
			}
			diags, err := client.Diagnostics(ctx, uri)
			if err != nil {
				return nil, err
			}
			cutoff := time.Now().Add(-diagnosticTTL)
			fresh := diags[:0]
			for _, diagnostic := range diags {
				if diagnostic.ReceivedAt.IsZero() || !diagnostic.ReceivedAt.Before(cutoff) {
					fresh = append(fresh, diagnostic)
				}
			}
			sort.SliceStable(fresh, func(i, j int) bool {
				if fresh[i].Severity != fresh[j].Severity {
					return fresh[i].Severity < fresh[j].Severity
				}
				if fresh[i].Range.Start.Line != fresh[j].Range.Start.Line {
					return fresh[i].Range.Start.Line < fresh[j].Range.Start.Line
				}
				if fresh[i].Range.Start.Character != fresh[j].Range.Start.Character {
					return fresh[i].Range.Start.Character < fresh[j].Range.Start.Character
				}
				return fresh[i].Message < fresh[j].Message
			})
			if len(fresh) > diagnosticLimit {
				fresh = fresh[:diagnosticLimit]
			}
			out := make([]map[string]any, 0, len(fresh))
			for _, d := range fresh {
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

func hoverTool(state *toolState) *tool.Definition {
	return positionTool("lsp_hover", "Get type information and documentation for a symbol at a position from the language server.", state,
		func(ctx context.Context, client ManagedClient, uri string, line, col int, _ map[string]any) (any, error) {
			result, err := client.Hover(ctx, uri, line, col)
			if err != nil {
				return nil, err
			}
			if result == nil {
				return map[string]any{"found": false}, nil
			}
			return map[string]any{"found": true, "contents": result.Contents}, nil
		})
}

func referencesTool(state *toolState) *tool.Definition {
	return positionTool("lsp_references", "Find all references to a symbol at a position - type-aware, with no false positives from same-name symbols.", state,
		func(ctx context.Context, client ManagedClient, uri string, line, col int, _ map[string]any) (any, error) {
			locs, err := client.References(ctx, uri, line, col)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(locs))
			for _, location := range locs {
				out = append(out, map[string]any{
					"file": location.URI,
					"line": location.Range.Start.Line + 1,
					"col":  location.Range.Start.Character + 1,
				})
			}
			return map[string]any{"references": out, "count": len(out)}, nil
		})
}

func renamePreviewTool(state *toolState) *tool.Definition {
	definition := positionTool("lsp_rename_preview", "Preview a rename across all files - see what would change, without applying it.", state,
		func(ctx context.Context, client ManagedClient, uri string, line, col int, args map[string]any) (any, error) {
			newName, _ := args["new_name"].(string)
			if newName == "" {
				return nil, fmt.Errorf("lsp_rename_preview: new_name is required")
			}
			edit, err := client.RenamePreview(ctx, uri, line, col, newName)
			if err != nil {
				return nil, err
			}
			if edit == nil {
				return map[string]any{"changes": nil}, nil
			}
			editCount := 0
			for _, edits := range edit.Changes {
				editCount += len(edits)
			}
			return map[string]any{"changes": edit.Changes, "file_count": len(edit.Changes), "edit_count": editCount}, nil
		})
	properties := definition.Parameters["properties"].(map[string]any)
	properties["new_name"] = map[string]any{"type": "string", "description": "The new name for the symbol"}
	definition.Parameters["required"] = []string{"file", "line", "character", "new_name"}
	return definition
}

func positionTool(name, description string, state *toolState, request func(context.Context, ManagedClient, string, int, int, map[string]any) (any, error)) *tool.Definition {
	return &tool.Definition{
		Name:        name,
		Description: description,
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
			if file == "" {
				return nil, fmt.Errorf("%s: file is required", name)
			}
			line, err := positiveIntArg(args["line"])
			if err != nil {
				return nil, fmt.Errorf("%s: line must be a positive integer", name)
			}
			col, err := positiveIntArg(args["character"])
			if err != nil {
				return nil, fmt.Errorf("%s: character must be a positive integer", name)
			}
			if name == "lsp_rename_preview" {
				newName, _ := args["new_name"].(string)
				if newName == "" {
					return nil, fmt.Errorf("lsp_rename_preview: new_name is required")
				}
			}
			client, uri, err := state.clientAndDocument(ctx, file)
			if err != nil {
				return nil, err
			}
			return request(ctx, client, uri, line-1, col-1, args)
		},
	}
}

func positiveIntArg(value any) (int, error) {
	var number int64
	switch value := value.(type) {
	case int:
		if value <= 0 {
			return 0, fmt.Errorf("not positive")
		}
		return value, nil
	case int64:
		number = value
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || value > float64(math.MaxInt) {
			return 0, fmt.Errorf("not an integer")
		}
		number = int64(value)
	case json.Number:
		parsed, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			return 0, err
		}
		number = parsed
	default:
		return 0, fmt.Errorf("not an integer")
	}
	if number <= 0 || uint64(number) > uint64(math.MaxInt) {
		return 0, fmt.Errorf("not positive")
	}
	return int(number), nil
}
