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
	"sync"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/document"
	"github.com/spawn08/chronos-code/internal/fsignore"
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

// Wrap installs bounded source reads and optional Go outlines. Relative paths
// resolve against root, as in the SDK. force is accepted for compatibility;
// every request now reads fresh content. It does not disable resource bounds.
func Wrap(a *agent.Agent, root string) {
	def, ok := a.Tools.Get(fileReadTool)
	if !ok {
		return
	}
	def.ParallelSafe = true
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
	props["offset"] = map[string]any{"type": "integer", "description": "First line, 1-indexed; a negative offset selects lines from the end. Use with limit instead of start_line/end_line."}
	props["limit"] = map[string]any{"type": "integer", "minimum": 1, "description": "Maximum lines with offset; defaults to 200 when offset is provided."}

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
		aliasRange := args["offset"] != nil || args["limit"] != nil
		offset := 1
		limit := 200
		if aliasRange {
			if args["start_line"] != nil || args["end_line"] != nil {
				return nil, fmt.Errorf("file_read: use either offset/limit or start_line/end_line")
			}
			if value, exists := args["offset"]; exists {
				switch n := value.(type) {
				case int:
					offset = n
				case float64:
					if math.Trunc(n) != n || n <= -float64(math.MaxInt) || n >= float64(math.MaxInt) {
						return nil, fmt.Errorf("file_read: invalid offset")
					}
					offset = int(n)
				default:
					return nil, fmt.Errorf("file_read: invalid offset")
				}
				if offset == 0 {
					return nil, fmt.Errorf("file_read: offset is 1-indexed; zero is invalid")
				}
			}
			limit, err = lineArg(args, "limit", 200)
			if err != nil {
				return nil, err
			}
			if offset > 0 {
				if offset > math.MaxInt-limit+1 {
					return nil, fmt.Errorf("file_read: range overflows")
				}
				start, end = offset, offset+limit-1
			}
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
		if aliasRange && offset < 0 {
			return readTailRange(ctx, resolved, offset, limit)
		}
		outline, explicit := args["outline_only"].(bool)
		canOutline := !aliasRange && args["start_line"] == nil && args["end_line"] == nil &&
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

// workspaceRoot is the canonical request workspace root, the base for ignore
// rules; it falls back to the configured root when canonicalization fails.
func workspaceRoot(ctx context.Context, configuredRoot string) string {
	root := builtins.WorkspaceRoot(ctx, configuredRoot)
	if canonical, err := canonicalPath(root); err == nil {
		return canonical
	}
	return root
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
	stats, err := scanFile(ctx, path, maxScanBytes, func(n int, line []byte) error {
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
		content.Write(line)
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
// DeniedFunc reports a file that must not appear in search results.
type DeniedFunc func(path string) bool

func WrapGrep(a *agent.Agent, root string, denied ...DeniedFunc) {
	def, ok := a.Tools.Get(fileGrepTool)
	if !ok {
		return
	}
	def.ParallelSafe = true
	rg := &ripgrep{}
	def.Description = "Search file contents in a file or directory tree. Respects .gitignore and skips dependency, build and cache directories of every ecosystem (node_modules, .venv, target, .next, dist-style build output, vendor, ...), binary files and symlinks. " +
		"Literal substring by default; regex=true for a regular expression. output_mode files_with_matches (default for directories) lists matching files, content returns matching lines, count returns per-file counts; use files_with_matches first, then read or search one file. " +
		"glob restricts files by name pattern (\"**/*.py\", \"*.{ts,tsx}\"). Bounded to 500 results and 256 KiB output; check truncated, and narrow path or glob when it is set."
	props := toolProperties(def)
	props["regex"] = map[string]any{
		"type":        "boolean",
		"description": "Treat pattern as a regular expression instead of a literal substring",
	}
	props["output_mode"] = map[string]any{
		"type":        "string",
		"enum":        []string{grepModeFiles, grepModeContent, grepModeCount},
		"description": "files_with_matches (default when path is a directory), content (default for a single file), or count",
	}
	props["glob"] = map[string]any{
		"type":        "string",
		"description": "Only search files whose root-relative path matches this pattern; ** spans directories, {a,b} alternates",
	}
	props["case_insensitive"] = map[string]any{
		"type":        "boolean",
		"description": "Match regardless of letter case",
	}

	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("file_grep: %w", err)
		}
		p, _ := args["path"].(string)
		pattern, _ := args["pattern"].(string)
		useRegex, _ := args["regex"].(bool)
		caseInsensitive, _ := args["case_insensitive"].(bool)
		glob, _ := args["glob"].(string)
		mode, _ := args["output_mode"].(string)
		if p == "" {
			p = "."
		}
		if pattern == "" {
			return nil, fmt.Errorf("file_grep: non-empty 'pattern' argument is required")
		}
		switch mode {
		case "", grepModeFiles, grepModeContent, grepModeCount:
		default:
			return nil, fmt.Errorf("file_grep: output_mode must be %s, %s or %s", grepModeFiles, grepModeContent, grepModeCount)
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

		var matcher func(line []byte) bool
		switch {
		case useRegex || caseInsensitive:
			expr := pattern
			if !useRegex {
				expr = regexp.QuoteMeta(pattern)
			}
			if caseInsensitive {
				expr = "(?i)" + expr
			}
			re, reErr := regexp.Compile(expr)
			if reErr != nil {
				return nil, fmt.Errorf("file_grep: invalid regex pattern: %w", reErr)
			}
			matcher = re.Match
		default:
			literal := []byte(pattern)
			matcher = func(line []byte) bool { return bytes.Contains(line, literal) }
		}
		if mode == "" {
			mode = grepModeContent
			if info.IsDir() {
				mode = grepModeFiles
			}
		}

		search := grepSearch{matcher: matcher, remaining: grepMaxScanBytes, matches: make([]map[string]any, 0),
			ignore: fsignore.New(workspaceRoot(ctx, root)), glob: glob, root: resolvedPath, mode: mode, counts: map[string]int{},
			denied: firstDenied(denied)}
		if info.IsDir() {
			search.recursive = true
			err = rg.search(ctx, &search, resolvedPath, pattern, useRegex, caseInsensitive)
			if errors.Is(err, errRipgrepUnusable) {
				search.reset()
				err = search.walk(ctx, resolvedPath, 0)
			}
		} else if info.Mode().IsRegular() {
			err = search.file(ctx, resolvedPath, false)
		} else {
			return nil, fmt.Errorf("file_grep: path must be a regular file or directory, not a symlink or special file")
		}
		if err != nil {
			return nil, fmt.Errorf("file_grep: %w", err)
		}
		return search.result(resolvedPath, pattern, info.IsDir()), nil
	}
}

// file_grep output modes.
const (
	grepModeFiles   = "files_with_matches"
	grepModeContent = "content"
	grepModeCount   = "count"
)

// result renders the search in its output mode. content keeps the original
// shape ({file, line_number, content} matches); the other modes list files.
func (s *grepSearch) result(path, pattern string, recursive bool) map[string]any {
	out := map[string]any{"path": path, "pattern": pattern, "recursive": recursive, "output_mode": s.mode, "truncated": s.truncated}
	switch s.mode {
	case grepModeFiles:
		out["files"] = s.files
	case grepModeCount:
		counts := make([]map[string]any, 0, len(s.files))
		for _, f := range s.files {
			counts = append(counts, map[string]any{"file": f, "count": s.counts[f]})
		}
		out["counts"] = counts
	default:
		out["matches"] = s.matches
	}
	return out
}

type grepSearch struct {
	matcher   func([]byte) bool
	ignore    *fsignore.Matcher
	denied    DeniedFunc // policy-denied files, skipped like ignored ones
	glob      string     // root-relative file filter; "" matches all
	root      string     // search root, for glob-relative paths
	mode      string
	recursive bool     // matches carry their file path
	files     []string // files with at least one match, in walk order
	counts    map[string]int
	matches   []map[string]any
	remaining int64
	output    int
	entries   int
	truncated bool
	stopped   bool
}

func (s *grepSearch) file(ctx context.Context, path string, recursive bool) error {
	before, outputBefore, filesBefore := len(s.matches), s.output, len(s.files)
	s.recursive = recursive
	if looksBinary(path) {
		// Sniff before scanning: files and count modes stop at the first
		// match and would otherwise never reach a later NUL byte.
		return nil
	}
	stats, err := scanFile(ctx, path, min(int64(maxScanBytes), s.remaining), func(n int, line []byte) error {
		if bytes.IndexByte(line, 0) >= 0 || !utf8.Valid(line) {
			return errBinary
		}
		if !s.matcher(line) {
			return nil
		}
		return s.addMatch(path, n, line)
	})
	s.remaining -= stats.bytes
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errBinary) {
		// Discard earlier text matches if a later scanned line identifies binary.
		s.matches, s.output, s.files = s.matches[:before], outputBefore, s.files[:filesBefore]
		delete(s.counts, path)
		return nil
	}
	if errors.Is(err, errScanLimit) || errors.Is(err, errLongLine) {
		s.truncated = true
		return nil
	}
	return err
}

// addMatch records one matching line within the output bounds, returning
// errStopScan once the search must stop or the file needs no more lines.
func (s *grepSearch) addMatch(path string, n int, line []byte) error {
	if s.mode == grepModeFiles || s.mode == grepModeCount {
		return s.recordFile(path)
	}
	// Allow for JSON escaping (up to six bytes per source byte), keys,
	// coordinates, and repeated file paths, not just matched text.
	cost := 6*len(line) + 80
	if s.recursive {
		cost += 6 * len(path)
	}
	if len(s.matches) >= grepMaxMatches || s.output+cost > maxOutputBytes {
		s.truncated, s.stopped = true, true
		return errStopScan
	}
	m := map[string]any{"line_number": n, "content": string(line)}
	if s.recursive {
		m["file"] = path
	}
	s.matches = append(s.matches, m)
	s.output += cost
	if len(s.matches) == grepMaxMatches {
		s.truncated, s.stopped = true, true
		return errStopScan
	}
	return nil
}

// recordFile notes a match for the files and count modes. In files mode the
// rest of the file is skipped; in count mode every line is counted.
func (s *grepSearch) recordFile(path string) error {
	if s.counts[path] == 0 {
		cost := 6*len(path) + 32
		if len(s.files) >= grepMaxMatches || s.output+cost > maxOutputBytes {
			s.truncated, s.stopped = true, true
			return errStopScan
		}
		s.files = append(s.files, path)
		s.output += cost
	}
	s.counts[path]++
	if s.mode == grepModeFiles {
		return errStopScan
	}
	return nil
}

// skipFile applies the glob filter to a file below the search root.
func (s *grepSearch) skipFile(path string) bool {
	if s.glob == "" {
		return false
	}
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return true
	}
	rel = filepath.ToSlash(rel)
	if fsignore.Match(s.glob, rel) {
		return false
	}
	// A pattern without a slash matches the base name anywhere, as in rg -g.
	return strings.Contains(s.glob, "/") || !fsignore.Match(s.glob, filepath.Base(path))
}

func (s *grepSearch) walk(ctx context.Context, path string, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 0 && s.ignored(path, true) {
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
			} else if entry.Type().IsRegular() && !s.skipFile(child) && !s.ignored(child, false) {
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

// reset clears partial results before a fallback search.
func (s *grepSearch) reset() {
	s.matches, s.files, s.counts = s.matches[:0], nil, map[string]int{}
	s.output, s.entries, s.truncated, s.stopped = 0, 0, false, false
	s.remaining = grepMaxScanBytes
}

// ignoredByMatcher drops rg results inside ecosystem directories that a
// nested .gitignore did not cover (rg sees only the --glob exclusions).
func ignoredByMatcher(s *grepSearch, path string) bool {
	return s.ignored(path, false)
}

func fsignoreDirNames() []string { return fsignore.DirNames() }

func firstDenied(denied []DeniedFunc) DeniedFunc {
	if len(denied) > 0 {
		return denied[0]
	}
	return nil
}

// ignored applies policy denials, ecosystem and .gitignore rules; a search
// without a matcher (tests driving walk directly) still skips ecosystem
// directories.
func (s *grepSearch) ignored(path string, isDir bool) bool {
	if !isDir && s.denied != nil && s.denied(path) {
		return true
	}
	if s.ignore == nil {
		return isDir && fsignore.SkipDirName(filepath.Base(path))
	}
	return s.ignore.Ignored(path, isDir)
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

// binarySniffBytes is how much of a file looksBinary inspects, as git and
// ripgrep do: a NUL byte in the first 8 KiB marks the file binary.
const binarySniffBytes = 8 << 10

func looksBinary(path string) bool {
	f, err := openRegular(path)
	if err != nil {
		return false // the scan reports the error
	}
	defer f.Close()
	buf := make([]byte, binarySniffBytes)
	n, _ := io.ReadFull(f, buf)
	return bytes.IndexByte(buf[:n], 0) >= 0
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

func scanFile(ctx context.Context, path string, limit int64, visit func(int, []byte) error) (scanStats, error) {
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

var scanReaders = sync.Pool{New: func() any {
	return bufio.NewReaderSize(nil, maxLineBytes+1)
}}

// visit borrows each line until it returns. Only retained matches/output need a
// copy; skipped lines never allocate. Readers are exclusive to one active scan.
func scanLines(ctx context.Context, input io.Reader, limit int64, visit func(int, []byte) error) (stats scanStats, err error) {
	r := &budgetReader{ctx: ctx, r: input, remaining: limit}
	defer func() { stats.bytes = limit - r.remaining }()
	reader := scanReaders.Get().(*bufio.Reader)
	reader.Reset(r)
	defer func() {
		reader.Reset(nil)
		scanReaders.Put(reader)
	}()
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
		visitErr := visit(n, line)
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
