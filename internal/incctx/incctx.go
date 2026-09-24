// Package incctx replaces the SDK's whole-file read/grep handlers with bounded
// incremental IO. It deliberately does not infer current context coverage from
// mtime: an earlier outline, range, or compacted turn is not the requested source.
//
// Install these wrappers on the builtins BEFORE handler middleware (hooks,
// compression, audit/security wrappers). Direct IO cannot delegate to an opaque
// whole-file handler without losing its bounds and source coordinates. Registry
// permissions/approval still execute outside the handler. These wrappers are not
// adapters for custom handlers with their own authorization or virtual files.
package incctx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/document"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
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

// Bounds apply even to force/full-content requests. A bounded prefix is marked
// truncated; an unrepresentable line or unreachable range produces an error.
const (
	maxLineBytes     = 64 << 10
	maxOutputBytes   = 256 << 10
	maxScanBytes     = 8 << 20
	maxOutlineBytes  = 1 << 20
	grepMaxScanBytes = 32 << 20
	grepMaxEntries   = 10000
	grepMaxDepth     = 64
)

var (
	errScanLimit = errors.New("scan byte limit reached; narrow the path or range")
	errLongLine  = errors.New("line exceeds 64 KiB; use a byte-oriented tool")
	errStopScan  = errors.New("requested scan complete")
	errBinary    = errors.New("binary file skipped")
)

// grepSkipDirs are directory names a recursive file_grep never descends
// into: version control internals and dependency trees are large, rarely
// what a caller is searching for, and expensive to walk.
var grepSkipDirs = map[string]bool{
	".git":          true,
	"node_modules":  true,
	"vendor":        true,
	".chronos-code": true,
	".hg":           true,
	".svn":          true,
	"__pycache__":   true,
	".venv":         true,
}

// Wrap installs bounded source reads and optional Go outlines. Relative paths
// resolve against root, as in the SDK. force is accepted for compatibility;
// every request now reads fresh content. It does not disable resource bounds.
func Wrap(a *agent.Agent, root string) {
	def, ok := a.Tools.Get(fileReadTool)
	if !ok {
		return
	}
	def.Description += " Pass start_line/end_line (positive, 1-indexed, inclusive) for source ranges. Reads are bounded to 256 KiB output, 64 KiB per line and 8 MiB scanned; truncated results include a continuation line."
	props := toolProperties(def)
	props["start_line"] = map[string]any{
		"type":        "integer",
		"minimum":     1,
		"description": "First line to return, 1-indexed inclusive. Omit to start from the beginning of the file.",
	}
	props["end_line"] = map[string]any{
		"type":        "integer",
		"minimum":     1,
		"description": "Last line to return, 1-indexed inclusive. Omit to read to the end of the file.",
	}

	props["force"] = map[string]any{"type": "boolean", "description": "Re-read fresh source (the default); resource bounds still apply."}
	props["outline_only"] = map[string]any{"type": "boolean", "description": "Request a Go declaration outline; false requests source. Explicit line ranges take precedence. Outlines are limited to 1 MiB input."}

	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("file_read: %w", err)
		}
		path, _ := args["path"].(string)
		if path == "" {
			return nil, fmt.Errorf("file_read: 'path' argument is required")
		}
		start, err := lineArg(args, "start_line", 1)
		if err != nil {
			return nil, err
		}
		end, err := lineArg(args, "end_line", 0)
		if err != nil {
			return nil, err
		}
		if end != 0 && end < start {
			return nil, fmt.Errorf("file_read: end_line must be >= start_line")
		}
		for _, key := range []string{"force", "outline_only"} {
			if v, exists := args[key]; exists {
				if _, ok := v.(bool); !ok {
					return nil, fmt.Errorf("file_read: %s must be a boolean", key)
				}
			}
		}
		resolved, err := resolvePath(ctx, root, path)
		if err != nil {
			return nil, fmt.Errorf("file_read: %w", err)
		}
		stat, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("file_read: %w", err)
		}
		if document.Supported(resolved) {
			return readDocumentRange(ctx, resolved, start, end)
		}
		outline, explicit := args["outline_only"].(bool)
		canOutline := args["start_line"] == nil && args["end_line"] == nil &&
			(!explicit || outline) && strings.HasSuffix(resolved, ".go") &&
			stat.Mode().IsRegular() && stat.Size() <= maxOutlineBytes &&
			(outline || stat.Size() > outlineSizeThreshold)
		if canOutline {
			decls, outlineErr := goOutline(ctx, resolved)
			if ctx.Err() != nil {
				return nil, fmt.Errorf("file_read: %w", ctx.Err())
			}
			if outlineErr == nil && len(decls) > 0 {
				return map[string]any{"path": resolved, "outline": true, "declarations": decls,
					"hint": "outline only; use start_line/end_line or outline_only=false for source"}, nil
			}
		}
		return readRange(ctx, resolved, start, end)
	}
}

func readDocumentRange(ctx context.Context, path string, start, end int) (any, error) {
	text, err := document.Extract(ctx, path, maxOutputBytes)
	if err != nil {
		return nil, fmt.Errorf("file_read %q: %w", path, err)
	}
	lines := strings.Split(text, "\n")
	if start > len(lines) {
		return nil, fmt.Errorf("file_read: start_line %d is beyond EOF (%d total lines)", start, len(lines))
	}
	last := len(lines)
	if end > 0 && end < last {
		last = end
	}
	return map[string]any{
		"path": path, "content": strings.Join(lines[start-1:last], "\n"),
		"start_line": start, "end_line": last, "total_lines": len(lines),
		"document": true, "hint": "text extracted from document; PDF layout and DOCX formatting are not preserved",
	}, nil
}

func resolvePath(ctx context.Context, configuredRoot, path string) (string, error) {
	workspaceRoot := builtins.WorkspaceRoot(ctx, configuredRoot)
	root, err := canonicalPath(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	if filepath.IsAbs(path) {
		if requestRoot, overridden := builtins.WorkspaceRootFromContext(ctx); overridden && configuredRoot != "" {
			configured, absErr := filepath.Abs(configuredRoot)
			if absErr == nil && pathWithin(configured, path) {
				relative, relErr := filepath.Rel(configured, path)
				if relErr == nil {
					path = filepath.Join(requestRoot, relative)
				}
			}
		}
	} else {
		path = filepath.Join(workspaceRoot, path)
	}
	candidate, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	resolved, err := canonicalPath(path)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if !pathWithin(root, resolved) {
		return "", fmt.Errorf("path %q is outside request workspace", path)
	}
	return filepath.Clean(candidate), nil
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)

}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// readRange stops at the requested end, rather than scanning the remainder just
// to count it. total_lines is nil unless EOF is observed (Split-style lines,
// including the empty final line after a trailing newline).
func readRange(ctx context.Context, path string, start, end int) (any, error) {
	var content strings.Builder
	last := start - 1
	truncated := false
	stats, err := scanFile(ctx, path, maxScanBytes, func(n int, line string) error {
		if n < start {
			return nil
		}
		separator := 0
		if last >= start {
			separator = 1
		}
		if content.Len()+separator+len(line) > maxOutputBytes {
			truncated = true
			return errStopScan
		}
		if separator != 0 {
			content.WriteByte('\n')
		}
		content.WriteString(line)
		last = n
		if end != 0 && n == end {
			return errStopScan
		}
		return nil
	})
	if errors.Is(err, errScanLimit) && last >= start {
		truncated = true
	} else if err != nil {
		return nil, fmt.Errorf("file_read %q: %w", path, err)
	}
	if last < start {
		return nil, fmt.Errorf("file_read: start_line %d is beyond EOF (%d total lines)", start, stats.lines)
	}
	result := map[string]any{"path": path, "content": content.String(), "start_line": start,
		"end_line": last, "total_lines": nil, "truncated": truncated}
	if stats.complete {
		result["total_lines"] = stats.lines
	}
	if truncated {
		result["next_start_line"] = last + 1
		result["hint"] = "bounded prefix; request a narrower range starting at next_start_line (8 MiB scan limit still applies)"
	}
	return result, nil
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

func lineArg(args map[string]any, key string, fallback int) (int, error) {
	if _, exists := args[key]; !exists {
		return fallback, nil
	}
	n := 0
	switch v := args[key].(type) {
	case float64:
		// float64(MaxInt) rounds up on 64-bit machines, so reject that edge.
		if v >= 1 && v < float64(math.MaxInt) && math.Trunc(v) == v {
			n = int(v)
		}
	case int:
		n = v
	}
	if n < 1 {
		return 0, fmt.Errorf("file_read: %s must be a positive integer within the supported line range", key)
	}
	return n, nil
}

// WrapGrep installs bounded literal/regex search for files and directories.
// Directory traversal is batched (not WalkDir's whole-directory sort), skips
// symlinks/special files, and has aggregate byte, entry, depth and output limits.
// Ignore names are explicit below; this is not a gitignore interpreter.
func WrapGrep(a *agent.Agent, root string) {
	def, ok := a.Tools.Get(fileGrepTool)
	if !ok {
		return
	}
	def.Description = "Search a file or directory recursively, skipping binary files, symlinks and .git/.hg/.svn/vendor/node_modules/.chronos-code/.venv/__pycache__ directories. Set regex=true for a regular expression; otherwise matches a literal substring. Bounded to 500 matches, 256 KiB output, 64 KiB lines, 8 MiB per file, 32 MiB total, 10000 entries and 64 directory levels. Check truncated for incomplete searches."
	toolProperties(def)["regex"] = map[string]any{
		"type":        "boolean",
		"description": "Treat pattern as a regular expression instead of a literal substring",
	}

	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("file_grep: %w", err)
		}
		p, _ := args["path"].(string)
		pattern, _ := args["pattern"].(string)
		useRegex, _ := args["regex"].(bool)
		if p == "" || pattern == "" {
			return nil, fmt.Errorf("file_grep: 'path' and non-empty 'pattern' arguments are required")
		}
		if len(pattern) > maxLineBytes {
			return nil, fmt.Errorf("file_grep: pattern exceeds 64 KiB")
		}
		resolvedPath, err := resolvePath(ctx, root, p)
		if err != nil {
			return nil, fmt.Errorf("file_grep: %w", err)
		}
		info, statErr := os.Lstat(resolvedPath)
		if statErr != nil {
			return nil, fmt.Errorf("file_grep: %w", statErr)
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

		search := grepSearch{matcher: matcher, remaining: grepMaxScanBytes, matches: make([]map[string]any, 0)}
		err = nil
		if info.IsDir() {
			err = search.walk(ctx, resolvedPath, 0)
		} else if info.Mode().IsRegular() {
			err = search.file(ctx, resolvedPath, false)
		} else {
			return nil, fmt.Errorf("file_grep: path must be a regular file or directory, not a symlink or special file")
		}
		if err != nil {
			return nil, fmt.Errorf("file_grep: %w", err)
		}
		return map[string]any{
			"path":      resolvedPath,
			"pattern":   pattern,
			"recursive": info.IsDir(),
			"matches":   search.matches,
			"truncated": search.truncated,
		}, nil
	}
}

type grepSearch struct {
	matcher   func(string) bool
	matches   []map[string]any
	remaining int64
	output    int
	entries   int
	truncated bool
	stopped   bool
}

func (s *grepSearch) file(ctx context.Context, path string, recursive bool) error {
	before, outputBefore := len(s.matches), s.output
	stats, err := scanFile(ctx, path, min(int64(maxScanBytes), s.remaining), func(n int, line string) error {
		if strings.IndexByte(line, 0) >= 0 || !utf8.ValidString(line) {
			return errBinary
		}
		if !s.matcher(line) {
			return nil
		}
		// Allow for JSON escaping (up to six bytes per source byte), keys,
		// coordinates, and repeated file paths, not just matched text.
		cost := 6*len(line) + 80
		if recursive {
			cost += 6 * len(path)
		}
		if len(s.matches) >= grepMaxMatches || s.output+cost > maxOutputBytes {
			s.truncated, s.stopped = true, true
			return errStopScan
		}
		m := map[string]any{"line_number": n, "content": line}
		if recursive {
			m["file"] = path
		}
		s.matches = append(s.matches, m)
		s.output += cost
		if len(s.matches) == grepMaxMatches {
			s.truncated, s.stopped = true, true
			return errStopScan
		}
		return nil
	})
	s.remaining -= stats.bytes
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errBinary) {
		// Discard earlier text matches if a later scanned line identifies binary.
		s.matches, s.output = s.matches[:before], outputBefore
		return nil
	}
	if errors.Is(err, errScanLimit) || errors.Is(err, errLongLine) {
		s.truncated = true
		return nil
	}
	return err
}

func (s *grepSearch) walk(ctx context.Context, path string, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if grepSkipDirs[filepath.Base(path)] {
		return nil
	}
	if depth >= grepMaxDepth {
		s.truncated = true
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.stopped || s.remaining <= 0 || s.entries >= grepMaxEntries {
			s.truncated, s.stopped = true, true
			return nil
		}
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if s.stopped || s.remaining <= 0 || s.entries >= grepMaxEntries {
				s.truncated, s.stopped = true, true
				return nil
			}
			s.entries++
			child := filepath.Join(path, entry.Name())
			var err error
			if entry.IsDir() {
				err = s.walk(ctx, child, depth+1)
			} else if entry.Type().IsRegular() {
				err = s.file(ctx, child, true)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				// Unreadable/disappeared entries make a recursive search partial.
				s.truncated = true
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

type scanStats struct {
	bytes    int64
	lines    int
	complete bool
}

// budgetReader checks cancellation at every buffered IO boundary and never
// consumes beyond its byte allowance. Exhaustion is not confused with EOF.
type budgetReader struct {
	ctx       context.Context
	r         io.Reader
	remaining int64
}

func (r *budgetReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.remaining <= 0 {
		return 0, errScanLimit
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func openRegular(path string) (*os.File, error) {
	// Reject devices/FIFOs before opening: a canceled context cannot interrupt
	// a blocked FIFO open. Regular filesystem syscalls themselves are synchronous.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%q is not a regular file", path)
	}
	return f, nil
}

func scanFile(ctx context.Context, path string, limit int64, visit func(int, string) error) (scanStats, error) {
	if err := ctx.Err(); err != nil {
		return scanStats{}, err
	}
	f, err := openRegular(path)
	if err != nil {
		return scanStats{}, err
	}
	defer f.Close()
	return scanLines(ctx, f, limit, visit)
}

func scanLines(ctx context.Context, input io.Reader, limit int64, visit func(int, string) error) (stats scanStats, err error) {
	r := &budgetReader{ctx: ctx, r: input, remaining: limit}
	defer func() { stats.bytes = limit - r.remaining }()
	reader := bufio.NewReaderSize(r, maxLineBytes+1)
	for n := 1; ; n++ {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		line, readErr := reader.ReadSlice('\n')
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			return stats, errLongLine
		}
		if readErr != nil && readErr != io.EOF {
			return stats, readErr
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		if len(line) > maxLineBytes {
			return stats, errLongLine
		}
		stats.lines, stats.complete = n, readErr == io.EOF
		visitErr := visit(n, string(line))
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if visitErr != nil {
			if errors.Is(visitErr, errStopScan) {
				return stats, nil
			}
			return stats, visitErr
		}
		if stats.complete {
			return stats, nil
		}
	}
}

// goOutline parses the Go source file at path and returns one string per
// top-level declaration it can confidently render: function signatures
// (without bodies) for *ast.FuncDecl, and rendered source (truncated if
// very long) for *ast.GenDecl (const/var/type groups). Declarations that
// can't be confidently rendered are skipped rather than aborting the whole
// outline. Returns (nil, err) on a genuine parse failure.
func goOutline(ctx context.Context, path string) ([]string, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Bound the parser input even if the file grows after stat. No unbounded
	// parser file read, and cancellation is checked during input and rendering.
	data, err := io.ReadAll(&budgetReader{ctx: ctx, r: f, remaining: maxOutlineBytes + 1})
	if err != nil {
		return nil, err
	}
	if len(data) > maxOutlineBytes {
		return nil, errScanLimit
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	outline := make([]string, 0, len(file.Decls))
	size := 0
	for _, decl := range file.Decls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if s, ok := renderDecl(fset, decl); ok {
			if len(s) > maxLineBytes || size+len(s) > maxOutputBytes {
				return nil, errScanLimit
			}
			outline = append(outline, s)
			size += len(s)
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
