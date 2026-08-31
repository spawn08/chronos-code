// Package incctx implements PRD P2-007 "Incremental context loading" and
// P2-008 "Semantic dedup + file content cache", both scoped to wrapping the
// "file_read" tool's Handler. The package is deliberately named incctx (not
// context) to avoid shadowing the stdlib context package in every file that
// imports both.
//
// Wrap intercepts file_read calls in two cases before delegating to the
// original handler: (1) if the file's mtime is unchanged since the last read
// in this conversation, it short-circuits with an "unchanged" marker instead
// of re-reading (P2-008); (2) if the file is a Go source file over 2000
// bytes and the caller didn't ask for a specific line range or full content,
// it returns an AST-derived outline of top-level declarations instead of the
// full file content (P2-007). Both cases fall through to the original
// handler whenever they can't confidently produce a result (stat failure,
// parse error, empty outline).
package incctx

import (
	"bytes"
	"context"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

// fileReadTool is the name of the tool Wrap intercepts.
const fileReadTool = "file_read"

// outlineSizeThreshold is the minimum file size (in bytes) at which Wrap
// prefers returning an outline over full file content (P2-007).
const outlineSizeThreshold = 2000

// Wrap wraps a's registered "file_read" tool handler so that unchanged files
// are served from an in-memory mtime cache instead of being re-read
// (P2-008), and large Go files are served as AST-aware outlines instead of
// full content by default (P2-007). root is used to resolve relative paths
// passed in the "path" argument, matching how the underlying file_read tool
// resolves them. Wrap is a no-op if a has no "file_read" tool registered.
func Wrap(a *agent.Agent, root string) {
	def, ok := a.Tools.Get(fileReadTool)
	if !ok {
		return
	}
	orig := def.Handler
	agentID := a.ID

	var mu sync.Mutex
	cache := make(map[string]map[string]int64) // cacheKey -> resolvedPath -> mtimeUnixNano

	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		path, _ := args["path"].(string)
		if path == "" {
			return orig(ctx, args)
		}

		resolvedPath := path
		if !filepath.IsAbs(path) {
			resolvedPath = filepath.Join(root, path)
		}

		force, _ := args["force"].(bool)
		if _, has := args["force"]; has {
			delete(args, "force")
		}

		stat, statErr := os.Stat(resolvedPath)
		if statErr != nil {
			return orig(ctx, args)
		}

		key := sessionOrAgent(ctx, agentID)
		mtime := stat.ModTime().UnixNano()

		if !force {
			mu.Lock()
			prevMtime, seen := cache[key][resolvedPath]
			mu.Unlock()
			if seen && prevMtime == mtime {
				return map[string]any{
					"unchanged": true,
					"path":      path,
					"mtime":     stat.ModTime().Format(time.RFC3339),
					"hint":      "content unchanged since last read; pass force=true to re-read anyway",
				}, nil
			}
		}

		outlineOnly, hasOutlineOnly := args["outline_only"].(bool)
		explicitlyFullContent := hasOutlineOnly && !outlineOnly
		canOutline := args["start_line"] == nil &&
			args["end_line"] == nil &&
			!explicitlyFullContent &&
			strings.HasSuffix(resolvedPath, ".go") &&
			stat.Size() > outlineSizeThreshold

		if canOutline {
			outline, outlineErr := goOutline(resolvedPath)
			if outlineErr == nil && len(outline) > 0 {
				mu.Lock()
				if cache[key] == nil {
					cache[key] = make(map[string]int64)
				}
				cache[key][resolvedPath] = mtime
				mu.Unlock()
				return map[string]any{
					"path":         path,
					"outline":      true,
					"declarations": outline,
					"hint":         "outline only (no function bodies); call file_read again with start_line/end_line, or outline_only=false, for full source",
				}, nil
			}
		}

		result, err := orig(ctx, args)
		if err == nil {
			mu.Lock()
			if cache[key] == nil {
				cache[key] = make(map[string]int64)
			}
			cache[key][resolvedPath] = mtime
			mu.Unlock()
		}
		return result, err
	}
}

// goOutline parses the Go source file at path and returns one string per
// top-level declaration it can confidently render: function signatures
// (without bodies) for *ast.FuncDecl, and rendered source (truncated if
// very long) for *ast.GenDecl (const/var/type groups). Declarations that
// can't be confidently rendered are skipped rather than aborting the whole
// outline. Returns (nil, err) on a genuine parse failure.
func goOutline(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	outline := make([]string, 0, len(file.Decls))
	for _, decl := range file.Decls {
		if s, ok := renderDecl(fset, decl); ok {
			outline = append(outline, s)
		}
	}
	return outline, nil
}

// renderDecl renders a single top-level declaration, recovering from any
// panic so one bad declaration can't abort the whole outline.
func renderDecl(fset *token.FileSet, decl ast.Decl) (s string, ok bool) {
	defer func() {
		if recover() != nil {
			s, ok = "", false
		}
	}()

	switch d := decl.(type) {
	case *ast.FuncDecl:
		cp := *d
		cp.Body = nil
		var buf bytes.Buffer
		if err := format.Node(&buf, fset, &cp); err != nil {
			return "", false
		}
		sig := buf.String()
		sig = strings.TrimSuffix(sig, "{\n}")
		sig = strings.TrimRight(sig, " \n{}")
		return sig, true
	case *ast.GenDecl:
		var buf bytes.Buffer
		if err := format.Node(&buf, fset, d); err != nil {
			return "", false
		}
		text := buf.String()
		const maxLen = 300
		if len(text) > maxLen {
			text = text[:maxLen] + "..."
		}
		return text, true
	default:
		return "", false
	}
}

// sessionOrAgent resolves a per-conversation cache key: the active session
// ID if one is present on ctx, falling back to the agent's own ID.
func sessionOrAgent(ctx context.Context, agentID string) string {
	if id := storage.SessionFromContext(ctx); id != "" {
		return id
	}
	return agentID
}
