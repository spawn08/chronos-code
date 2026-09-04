// Package incctx implements PRD P2-007 "Incremental context loading" and
// P2-008 "Semantic dedup + file content cache", both scoped to wrapping the
// "file_read" and "file_grep" tools' Handlers. The package is deliberately
// named incctx (not context) to avoid shadowing the stdlib context package
// in every file that imports both.
//
// The chronos SDK's builtin file_read/file_grep are intentionally minimal
// (single-file, whole-file, plain substring). Rather than changing the
// shared SDK for every consumer, this package upgrades chronos-code's own
// copies of those tools in place — after agent.BuildAgent/BuildAll register
// the builtins, Wrap/WrapGrep swap in a richer Handler and extend the
// declared JSON Schema Parameters so the model can discover the new
// arguments, without chronos itself knowing this happened.
//
// Wrap intercepts file_read calls in three cases before delegating to the
// original handler: (1) if the file's mtime is unchanged since the last read
// in this conversation, it short-circuits with an "unchanged" marker instead
// of re-reading (P2-008); (2) if the file is a Go source file over 2000
// bytes and the caller didn't ask for a specific line range or full content,
// it returns an AST-derived outline of top-level declarations instead of the
// full file content (P2-007); (3) if the caller passed start_line/end_line,
// the full content is read once and then sliced to that range before being
// returned — the underlying SDK handler has no notion of line ranges, so
// this slicing happens entirely on the chronos-code side. All cases fall
// through to the original handler whenever they can't confidently produce a
// result (stat failure, parse error, empty outline).
//
// WrapGrep extends file_grep the same way: the underlying SDK handler only
// searches one file with a plain substring match. WrapGrep adds recursive
// directory search (skipping .git/vendor/node_modules) and an optional
// regex=true mode, both implemented in chronos-code and layered on top of
// the SDK's single-file substring search via the original handler.
package incctx

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

// fileReadTool is the name of the tool Wrap intercepts.
const fileReadTool = "file_read"

// fileGrepTool is the name of the tool WrapGrep intercepts.
const fileGrepTool = "file_grep"

// outlineSizeThreshold is the minimum file size (in bytes) at which Wrap
// prefers returning an outline over full file content (P2-007).
const outlineSizeThreshold = 2000

// grepMaxMatches caps the number of matches a single file_grep call returns,
// so a recursive search can't blow up the response size.
const grepMaxMatches = 500

// grepSkipDirs are directory names a recursive file_grep never descends
// into: version control internals and dependency trees are large, rarely
// what a caller is searching for, and expensive to walk.
var grepSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
}

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

	def.Description += " Pass start_line/end_line (1-indexed, inclusive) to read only part of a large file instead of the whole thing."
	props := toolProperties(def)
	props["start_line"] = map[string]any{
		"type":        "integer",
		"description": "First line to return, 1-indexed inclusive. Omit to start from the beginning of the file.",
	}
	props["end_line"] = map[string]any{
		"type":        "integer",
		"description": "Last line to return, 1-indexed inclusive. Omit to read to the end of the file.",
	}

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
		if err != nil {
			return result, err
		}

		startArg, hasStart := intArg(args, "start_line")
		endArg, hasEnd := intArg(args, "end_line")
		if resMap, ok := result.(map[string]any); ok && (hasStart || hasEnd) {
			if content, ok := resMap["content"].(string); ok {
				lines := strings.Split(content, "\n")
				total := len(lines)
				start, end := clampLineRange(startArg, hasStart, endArg, hasEnd, total)
				resMap["content"] = strings.Join(lines[start-1:end], "\n")
				resMap["start_line"] = start
				resMap["end_line"] = end
				resMap["total_lines"] = total
			}
		}

		mu.Lock()
		if cache[key] == nil {
			cache[key] = make(map[string]int64)
		}
		cache[key][resolvedPath] = mtime
		mu.Unlock()
		return result, err
	}
}

// toolProperties returns def's JSON Schema "properties" map, initializing
// def.Parameters and/or "properties" first if either is missing so callers
// can always add a new declared argument in place.
func toolProperties(def *tool.Definition) map[string]any {
	if def.Parameters == nil {
		def.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	props, ok := def.Parameters["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		def.Parameters["properties"] = props
	}
	return props
}

// intArg extracts an integer argument that may have been decoded as any of
// Go's JSON-numeric types (float64 from encoding/json, or a plain int from a
// caller that built args directly).
func intArg(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	default:
		return 0, false
	}
}

// clampLineRange resolves a requested (start, end) line range against a
// file's total line count, defaulting an omitted bound to the corresponding
// edge of the file and clamping both to [1, total].
func clampLineRange(start int, hasStart bool, end int, hasEnd bool, total int) (int, int) {
	if !hasStart {
		start = 1
	}
	if !hasEnd {
		end = total
	}
	if start < 1 {
		start = 1
	}
	if end > total {
		end = total
	}
	if end < start {
		end = start
	}
	return start, end
}

// WrapGrep wraps a's registered "file_grep" tool handler so that, in
// addition to the SDK's single-file substring search, path may be a
// directory (searched recursively, skipping grepSkipDirs) and pattern may be
// a regular expression when regex=true is passed. root is used to resolve
// relative paths, matching how the underlying file_grep tool resolves them.
// WrapGrep is a no-op if a has no "file_grep" tool registered.
func WrapGrep(a *agent.Agent, root string) {
	def, ok := a.Tools.Get(fileGrepTool)
	if !ok {
		return
	}
	orig := def.Handler

	def.Description = "Search for a pattern in a file or, if path is a directory, recursively in every file beneath it (skipping .git, vendor, node_modules). Set regex=true to treat pattern as a regular expression (supports alternation like \"foo|bar\"); otherwise it's a plain substring match."
	toolProperties(def)["regex"] = map[string]any{
		"type":        "boolean",
		"description": "Treat pattern as a regular expression instead of a literal substring",
	}

	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		p, _ := args["path"].(string)
		pattern, _ := args["pattern"].(string)
		useRegex, _ := args["regex"].(bool)
		if p == "" || pattern == "" {
			return orig(ctx, args)
		}

		resolvedPath := p
		if !filepath.IsAbs(p) {
			resolvedPath = filepath.Join(root, p)
		}

		info, statErr := os.Stat(resolvedPath)
		if statErr != nil {
			if !useRegex {
				// Defer to the SDK's own error path for a plain search.
				return orig(ctx, args)
			}
			return nil, fmt.Errorf("file_grep: %w", statErr)
		}
		if !info.IsDir() && !useRegex {
			// Plain substring search on a single file: the SDK handler
			// already does exactly this.
			return orig(ctx, args)
		}

		var matcher func(line string) bool
		if useRegex {
			re, reErr := regexp.Compile(pattern)
			if reErr != nil {
				return nil, fmt.Errorf("file_grep: invalid regex pattern: %w", reErr)
			}
			matcher = re.MatchString
		} else {
			matcher = func(line string) bool { return strings.Contains(line, pattern) }
		}

		if !info.IsDir() {
			matches, err := grepFile(resolvedPath, matcher, grepMaxMatches)
			if err != nil {
				return nil, fmt.Errorf("file_grep: %w", err)
			}
			return map[string]any{"path": resolvedPath, "pattern": pattern, "matches": matches}, nil
		}

		var matches []map[string]any
		truncated := false
		walkErr := filepath.WalkDir(resolvedPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if grepSkipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if len(matches) >= grepMaxMatches {
				truncated = true
				return filepath.SkipAll
			}
			fileMatches, err := grepFile(path, matcher, grepMaxMatches-len(matches))
			if err != nil {
				return nil
			}
			for _, m := range fileMatches {
				m["file"] = path
				matches = append(matches, m)
			}
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("file_grep: %w", walkErr)
		}
		return map[string]any{
			"path":      resolvedPath,
			"pattern":   pattern,
			"recursive": true,
			"matches":   matches,
			"truncated": truncated,
		}, nil
	}
}

// grepFile scans path line by line and returns up to maxMatches lines
// satisfying matcher.
func grepFile(path string, matcher func(string) bool, maxMatches int) ([]map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	var matches []map[string]any
	for i, line := range lines {
		if len(matches) >= maxMatches {
			break
		}
		if matcher(line) {
			matches = append(matches, map[string]any{
				"line_number": i + 1,
				"content":     line,
			})
		}
	}
	return matches, nil
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
