package graph

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cespare/xxhash/v2"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
)

const (
	evidenceReadLimit      = 256 * 1024
	evidenceFileReadLimit  = 64 * 1024
	evidenceExcerptLimit   = 4096
	evidenceSelectionLimit = 16
	evidenceItemLimit      = 64
)

type evidenceRange struct {
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type evidenceRequest struct {
	query     string
	symbols   []string
	ranges    []evidenceRange
	include   map[string]bool
	maxTokens int
}

type evidenceSource struct {
	Text          string `json:"text,omitempty"`
	StartLine     int    `json:"start_line,omitempty"`
	EndLine       int    `json:"end_line,omitempty"`
	IndexedHash   string `json:"indexed_hash,omitempty"`
	ExcerptSHA256 string `json:"excerpt_sha256,omitempty"`
	Freshness     string `json:"freshness"`
	Truncated     bool   `json:"truncated,omitempty"`
}

type evidenceItem struct {
	Role        string          `json:"role"`
	Name        string          `json:"name,omitempty"`
	Target      string          `json:"target,omitempty"`
	Kind        SymbolKind      `json:"kind,omitempty"`
	Package     string          `json:"package,omitempty"`
	File        string          `json:"file"`
	StartLine   int             `json:"start_line"`
	EndLine     int             `json:"end_line"`
	Signature   string          `json:"signature,omitempty"`
	IndexedHash string          `json:"indexed_hash,omitempty"`
	Source      *evidenceSource `json:"source,omitempty"`
}

type evidenceResult struct {
	// Revision is Git HEAD at query time, not a clean-worktree assertion.
	// IndexedHash and ExcerptSHA256 identify the indexed/observed source bytes.
	Revision  string         `json:"revision"`
	Counter   string         `json:"counter"`
	MaxTokens int            `json:"max_tokens"`
	Items     []evidenceItem `json:"items"`
	Omitted   map[string]int `json:"omitted"`
	Truncated bool           `json:"truncated"`
	IOBytes   int            `json:"io_bytes"`
}

// evidenceCounter is shared process-wide: building the BPE codec compiles a
// large regexp, and request-scoped tool definitions are rebuilt per graph.
// Counter access is serialized rather than assuming the SDK codec is safe for
// concurrent calls. Source/SQLite work can still proceed concurrently.
var evidenceCounter = sync.OnceValues(func() (model.TokenCounter, string) {
	if bpe, err := model.NewBPECounter(""); err == nil {
		return bpe, "sdk:o200k_base+10%+32"
	}
	// Never fall back to the SDK's default 4-bytes/token guess.
	return &model.EstimatingCounter{CharsPerToken: 1}, "sdk:bytes+10%+32"
})

var evidenceCounterMu sync.Mutex

func codebaseContextTool(store Backend, root string) *tool.Definition {
	return &tool.Definition{
		Name:        "codebase_context",
		Effects:     []tool.Effect{tool.EffectRead},
		Description: "Read-only, one-turn graph evidence: definitions, bounded source excerpts, direct callers and graph-reachable tests (depth 3). Exact/explicit matches precede FTS relevance; ties use source location. max_tokens bounds the complete compact JSON result using the SDK tokenizer plus overhead. Omissions and source freshness are explicit; relationships remain name-based.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"query":      map[string]any{"type": "string", "maxLength": 1024, "description": "Exact symbol name or FTS query; optional when symbols or ranges are supplied"},
				"max_tokens": map[string]any{"type": "integer", "minimum": 256, "maximum": 16384, "default": 4096},
				"include":    map[string]any{"type": "array", "minItems": 1, "maxItems": 4, "uniqueItems": true, "items": map[string]any{"type": "string", "enum": []string{"definitions", "callers", "tests", "excerpts"}}, "description": "Defaults to all four sections"},
				"symbols":    map[string]any{"type": "array", "minItems": 1, "maxItems": 16, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "description": "Batch of exact symbol names"},
				"ranges": map[string]any{"type": "array", "minItems": 1, "maxItems": 16, "items": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"file", "start_line", "end_line"},
					"properties": map[string]any{
						"file":       map[string]any{"type": "string", "minLength": 1, "maxLength": 4096},
						"start_line": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000000},
						"end_line":   map[string]any{"type": "integer", "minimum": 1, "maximum": 1000000, "description": "Inclusive; at most 200 requested lines per range"},
					},
				}},
			},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			rootAbs, err := filepath.Abs(root)
			if err != nil || root == "" {
				return nil, fmt.Errorf("codebase_context: workspace root is required")
			}
			req, err := parseEvidenceRequest(rootAbs, args)
			if err != nil {
				return nil, err
			}
			counter, basis := evidenceCounter()
			result, err := collectEvidence(ctx, store, rootAbs, req)
			if err != nil {
				return nil, err
			}
			result.Counter = basis
			evidenceCounterMu.Lock()
			defer evidenceCounterMu.Unlock()
			if err := fitEvidence(ctx, result, counter); err != nil {
				return nil, err
			}
			return result, nil
		},
	}
}

func evidenceInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		if n >= 0 && n <= 1000000 {
			return int(n), true
		}
	case float64:
		if !math.IsNaN(n) && n >= 0 && n <= 1000000 && math.Trunc(n) == n {
			return int(n), true
		}
	case json.Number:
		if i, err := n.Int64(); err == nil && i >= 0 && i <= 1000000 {
			return int(i), true
		}
	}
	return 0, false
}

func evidenceStrings(v any, limit int) ([]string, bool) {
	var out []string
	switch values := v.(type) {
	case []string:
		out = append(out, values...)
	case []any:
		for _, value := range values {
			s, ok := value.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
	default:
		return nil, false
	}
	if len(out) < 1 || len(out) > limit {
		return nil, false
	}
	for i, s := range out {
		out[i] = strings.TrimSpace(s)
		if out[i] == "" || len(out[i]) > 256 {
			return nil, false
		}
	}
	sort.Strings(out)
	return out, true
}

func evidenceRelative(root, path string) (string, bool) {
	if path == "" || len(path) > 4096 || strings.ContainsRune(path, 0) {
		return "", false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if !isPathWithin(root, path) {
		return "", false
	}
	rel, err := filepath.Rel(root, path)
	return rel, err == nil
}

func parseEvidenceRequest(root string, args map[string]any) (evidenceRequest, error) {
	r := evidenceRequest{maxTokens: 4096, include: map[string]bool{"definitions": true, "callers": true, "tests": true, "excerpts": true}}
	invalid := func() (evidenceRequest, error) {
		return r, fmt.Errorf("codebase_context: invalid arguments; check query, include, symbols, ranges and max_tokens bounds")
	}
	for key := range args {
		switch key {
		case "query", "max_tokens", "include", "symbols", "ranges":
		default:
			return invalid()
		}
	}
	if v, exists := args["query"]; exists {
		q, ok := v.(string)
		if !ok || len(q) > 1024 {
			return invalid()
		}
		r.query = strings.TrimSpace(q)
	}
	if v, exists := args["max_tokens"]; exists {
		n, ok := evidenceInt(v)
		if !ok || n < 256 || n > 16384 {
			return invalid()
		}
		r.maxTokens = n
	}
	if v, exists := args["include"]; exists {
		values, ok := evidenceStrings(v, 4)
		if !ok {
			return invalid()
		}
		r.include = make(map[string]bool)
		for _, value := range values {
			switch value {
			case "definitions", "callers", "tests", "excerpts":
				if r.include[value] {
					return invalid()
				}
				r.include[value] = true
			default:
				return invalid()
			}
		}
	}
	if v, exists := args["symbols"]; exists {
		values, ok := evidenceStrings(v, 16)
		if !ok {
			return invalid()
		}
		r.symbols = values
	}
	if v, exists := args["ranges"]; exists {
		values, ok := v.([]any)
		if !ok || len(values) < 1 || len(values) > 16 {
			return invalid()
		}
		for _, value := range values {
			obj, ok := value.(map[string]any)
			if !ok || len(obj) != 3 {
				return invalid()
			}
			file, ok := obj["file"].(string)
			if !ok {
				return invalid()
			}
			file, ok = evidenceRelative(root, file)
			if !ok {
				return invalid()
			}
			start, a := evidenceInt(obj["start_line"])
			end, b := evidenceInt(obj["end_line"])
			if !a || !b || start < 1 || end < start || end > 1000000 || end-start >= 200 {
				return invalid()
			}
			r.ranges = append(r.ranges, evidenceRange{file, start, end})
		}
		sort.Slice(r.ranges, func(i, j int) bool {
			a, b := r.ranges[i], r.ranges[j]
			if a.File != b.File {
				return a.File < b.File
			}
			if a.StartLine != b.StartLine {
				return a.StartLine < b.StartLine
			}
			return a.EndLine < b.EndLine
		})
	}
	if r.query == "" && len(r.symbols) == 0 && len(r.ranges) == 0 {
		return invalid()
	}
	return r, nil
}

type evidenceMatch struct {
	Symbol
	tier  int
	score float64
}

const evidenceColumns = `s.id, s.name, s.kind, s.package, s.file, s.line, s.end_line, s.signature, '', s.receiver`

// evidenceCallersQuery resolves callers of a short callee name to their
// declarations. edges.from_name is the caller's qualified identity, joined
// through the symbols expression index (see symbolQualifiedExpr) rather than a
// disjunction SQLite can only evaluate by scanning every symbol per edge.
var evidenceCallersQuery = `SELECT DISTINCT ` + evidenceColumns + `, 0 FROM edges e JOIN symbols s ON ` +
	symbolQualified("s.") +
	` = e.from_name AND (e.source_file = '' OR s.file = e.source_file) WHERE e.kind = 'call' AND e.to_name = ? ORDER BY s.file, s.line, s.name, s.id LIMIT 17`

// evidenceMatches orders symbols the way the evidence queries always have
// (by package first unless byFile), keeps the first 17 (one past the
// selection limit, so overflow is counted), and drops docs, which evidence
// never reports.
func evidenceMatches(syms []Symbol, byFile bool) []evidenceMatch {
	sort.SliceStable(syms, func(i, j int) bool {
		a, b := syms[i], syms[j]
		if !byFile && a.Package != b.Package {
			return a.Package < b.Package
		}
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	if len(syms) > evidenceSelectionLimit+1 {
		syms = syms[:evidenceSelectionLimit+1]
	}
	out := make([]evidenceMatch, len(syms))
	for i, sym := range syms {
		sym.Doc = ""
		out[i] = evidenceMatch{Symbol: sym}
	}
	return out
}

func evidenceRevision(ctx context.Context, root string) string {
	if revision, ok := readGitHead(root); ok {
		return revision
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "git", "--no-optional-locks", "-C", root, "rev-parse", "--verify", "HEAD").Output()
	if err != nil {
		return "unavailable"
	}
	revision := strings.TrimSpace(string(data))
	if len(revision) != 40 && len(revision) != 64 {
		return "unavailable"
	}
	if _, err := hex.DecodeString(revision); err != nil {
		return "unavailable"
	}
	return revision
}

// readGitHead resolves HEAD for a repository rooted exactly at root by reading
// Git's files instead of spawning `git rev-parse` on every request. Loose refs
// take precedence over packed-refs, as in Git. Anything unusual (no .git at
// root, reftable, unborn branch) reports ok=false so the caller falls back to
// the git binary.
func readGitHead(root string) (string, bool) {
	gitDir := filepath.Join(root, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return "", false
	}
	if !info.IsDir() {
		data, err := os.ReadFile(gitDir)
		if err != nil {
			return "", false
		}
		dir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
		if !ok {
			return "", false
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		gitDir = dir
	}
	commonDir := gitDir
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		dir := strings.TrimSpace(string(data))
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(gitDir, dir)
		}
		commonDir = dir
	}
	if _, err := os.Stat(filepath.Join(commonDir, "reftable")); err == nil {
		return "", false
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", false
	}
	value := strings.TrimSpace(string(head))
	ref, symbolic := strings.CutPrefix(value, "ref: ")
	if !symbolic {
		return value, validGitRevision(value)
	}
	if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") {
		return "", false
	}
	if data, err := os.ReadFile(filepath.Join(commonDir, filepath.FromSlash(ref))); err == nil {
		revision := strings.TrimSpace(string(data))
		return revision, validGitRevision(revision)
	}
	packed, err := os.ReadFile(filepath.Join(commonDir, "packed-refs"))
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(packed), "\n") {
		if revision, name, ok := strings.Cut(strings.TrimSpace(line), " "); ok && name == ref {
			return revision, validGitRevision(revision)
		}
	}
	return "", false
}

func validGitRevision(revision string) bool {
	if len(revision) != 40 && len(revision) != 64 {
		return false
	}
	_, err := hex.DecodeString(revision)
	return err == nil
}

func collectEvidence(ctx context.Context, store Backend, root string, req evidenceRequest) (*evidenceResult, error) {
	result := &evidenceResult{Revision: evidenceRevision(ctx, root), MaxTokens: req.maxTokens, Items: []evidenceItem{}, Omitted: make(map[string]int)}
	if result.Revision == "unavailable" {
		result.Omitted["revision_unavailable"]++
	}
	var matches []evidenceMatch
	addMatches := func(found []evidenceMatch, tier int) {
		if len(found) > evidenceSelectionLimit {
			result.Omitted["selection_limit"] += len(found) - evidenceSelectionLimit
			found = found[:evidenceSelectionLimit]
		}
		for _, m := range found {
			m.tier = tier
			matches = append(matches, m)
		}
	}
	exact := func(name string, tier int) error {
		syms, err := store.FindSymbols(ctx, name, "")
		if err != nil {
			return fmt.Errorf("query graph evidence: %w", err)
		}
		found := evidenceMatches(syms, false)
		if len(found) == 0 && tier == 0 {
			result.Omitted["not_found"]++
		}
		addMatches(found, tier)
		return nil
	}
	for i, name := range req.symbols {
		if i > 0 && name == req.symbols[i-1] {
			continue
		}
		if err := exact(name, 0); err != nil {
			return nil, err
		}
	}
	if req.query != "" {
		beforeQuery := len(matches)
		if err := exact(req.query, 1); err != nil {
			return nil, err
		}
		if query := fts5MatchQuery(req.query); query != "" {
			results, err := store.Search(ctx, req.query, evidenceSelectionLimit+1)
			if err != nil {
				return nil, fmt.Errorf("query graph evidence: %w", err)
			}
			found := make([]evidenceMatch, len(results))
			for i, r := range results {
				r.Doc = ""
				found[i] = evidenceMatch{Symbol: r.Symbol, score: r.Rank}
			}
			addMatches(found, 2)
		}
		if len(matches) == beforeQuery {
			result.Omitted["not_found"]++
		}
	}
	for _, span := range req.ranges {
		var inRange []Symbol
		for _, file := range []string{span.File, filepath.Join(root, span.File)} {
			syms, err := store.SymbolsInFile(ctx, file)
			if err != nil {
				return nil, fmt.Errorf("query graph evidence: %w", err)
			}
			for _, sym := range syms {
				if sym.Line <= span.EndLine && max(sym.Line, sym.EndLine) >= span.StartLine {
					inRange = append(inRange, sym)
				}
			}
		}
		addMatches(evidenceMatches(inRange, true), 0)
		result.Items = append(result.Items, evidenceItem{Role: "range", File: filepath.ToSlash(span.File), StartLine: span.StartLine, EndLine: span.EndLine})
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.score != b.score {
			return a.score < b.score
		}
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	seen := make(map[int64]bool)
	seenItems := make(map[string]bool)
	addItem := func(sym Symbol, role, target string) error {
		file, ok := evidenceRelative(root, sym.File)
		if !ok || len(sym.Name) > 256 || len(sym.Package) > 1024 {
			result.Omitted["invalid_location_or_metadata"]++
			return nil
		}
		key := fmt.Sprintf("%s:%d:%s", role, sym.ID, target)
		if seenItems[key] {
			return nil
		}
		seenItems[key] = true
		if len(result.Items) >= evidenceItemLimit {
			result.Omitted["item_limit"]++
			return nil
		}
		signature := sym.Signature
		if len(signature) > 512 {
			signature = evidenceClip(signature, 512)
			result.Omitted["metadata_limit"]++
		}
		indexedHash, err := store.FileHash(ctx, sym.File)
		if err != nil {
			return err
		}
		if indexedHash == "" {
			result.Omitted["unindexed_graph"]++
		}
		result.Items = append(result.Items, evidenceItem{Role: role, Name: sym.Name, Target: target, Kind: sym.Kind, Package: sym.Package, File: filepath.ToSlash(file), StartLine: sym.Line, EndLine: max(sym.Line, sym.EndLine), Signature: signature, IndexedHash: indexedHash})
		return nil
	}
	lookups := 0
	for _, match := range matches {
		if seen[match.ID] {
			continue
		}
		seen[match.ID] = true
		if len(seen) > evidenceSelectionLimit {
			result.Omitted["selection_limit"]++
			continue
		}
		if req.include["definitions"] || req.include["excerpts"] {
			if err := addItem(match.Symbol, "definition", ""); err != nil {
				return nil, err
			}
		}
		if !req.include["callers"] && !req.include["tests"] {
			continue
		}
		frontier := []string{match.Name}
		visited := map[string]bool{match.Name: true}
		for depth := 0; depth < 3 && len(frontier) > 0; depth++ {
			var next []string
			for _, target := range frontier {
				if lookups >= 64 {
					result.Omitted["relationship_limit"]++
					break
				}
				lookups++
				callerSyms, err := store.CallerSymbols(ctx, target, evidenceSelectionLimit+1)
				if err != nil {
					return nil, err
				}
				callers := make([]evidenceMatch, len(callerSyms))
				for i, sym := range callerSyms {
					sym.Doc = ""
					callers[i] = evidenceMatch{Symbol: sym}
				}
				if len(callers) > 16 {
					result.Omitted["relationship_limit"]++
					callers = callers[:16]
				}
				for _, caller := range callers {
					if depth == 0 && req.include["callers"] {
						if err := addItem(caller.Symbol, "caller", match.Name); err != nil {
							return nil, err
						}
					}
					if req.include["tests"] && evidenceIsTest(caller.Symbol) {
						if err := addItem(caller.Symbol, "test", match.Name); err != nil {
							return nil, err
						}
					}
					if !visited[caller.Name] {
						visited[caller.Name] = true
						next = append(next, caller.Name)
					}
				}
			}
			if !req.include["tests"] {
				break
			}
			sort.Strings(next)
			frontier = next
		}
		if req.include["tests"] && len(frontier) > 0 {
			result.Omitted["test_depth_limit"]++
		}
	}
	if req.include["excerpts"] && len(result.Items) > 0 {
		fs, err := os.OpenRoot(root)
		if err != nil {
			return nil, fmt.Errorf("open evidence root: %w", err)
		}
		defer fs.Close()
		remaining := evidenceReadLimit
		for i := range result.Items {
			item := &result.Items[i]
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			end := item.EndLine
			if item.Role != "range" && end-item.StartLine >= 64 {
				end = item.StartLine + 63
				result.Omitted["excerpt_limit"]++
			}
			source, reason, err := readEvidenceSource(ctx, store, fs, root, item.File, max(1, item.StartLine), max(1, end), &remaining)
			if err != nil {
				return nil, err
			}
			if end != item.EndLine {
				source.Truncated = true
			}
			item.Source = source
			if reason != "" {
				result.Omitted[reason]++
			}
		}
		result.IOBytes = evidenceReadLimit - remaining
	}
	result.Truncated = len(result.Omitted) > 0
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func evidenceIsTest(sym Symbol) bool {
	file := filepath.ToSlash(sym.File)
	if strings.HasSuffix(file, "_test.go") {
		return strings.HasPrefix(sym.Name, "Test") || strings.HasPrefix(sym.Name, "Example") || strings.HasPrefix(sym.Name, "Fuzz") || strings.HasPrefix(sym.Name, "Benchmark")
	}
	return strings.HasPrefix(sym.Name, "test_") && (strings.HasPrefix(filepath.Base(file), "test_") || strings.HasSuffix(file, "_test.py"))
}

var errEvidenceReadLimit = errors.New("evidence read limit")

type evidenceReader struct {
	ctx       context.Context
	file      io.Reader
	remaining *int
	n         int
	hash      hash.Hash64
}

func (r *evidenceReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	limit := min(len(p), *r.remaining, evidenceFileReadLimit-r.n)
	if limit <= 0 {
		return 0, errEvidenceReadLimit
	}
	n, err := r.file.Read(p[:limit])
	r.n += n
	*r.remaining -= n
	_, _ = r.hash.Write(p[:n])
	return n, err
}

func readEvidenceSource(ctx context.Context, store Backend, fs *os.Root, root, file string, start, end int, remaining *int) (*evidenceSource, string, error) {
	source := &evidenceSource{Freshness: "unverified"}
	indexedHash, indexedMtime, err := store.FileProvenance(ctx, file, filepath.Join(root, file))
	if err != nil {
		return nil, "", err
	}
	source.IndexedHash = indexedHash
	// Root performs symlink containment during lookup/open, rather than using
	// a check-then-open absolute path that could be redirected outside root.
	info, err := fs.Stat(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			source.Freshness = "missing"
			return source, "missing_source", nil
		}
		source.Freshness = "unavailable"
		return source, "unavailable_source", nil
	}
	if !info.Mode().IsRegular() {
		source.Freshness = "unavailable"
		return source, "non_regular_source", nil
	}
	f, err := fs.Open(file)
	if err != nil {
		source.Freshness = "unavailable"
		return source, "unavailable_source", nil
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() {
		source.Freshness = "unavailable"
		return source, "unavailable_source", nil
	}
	r := &evidenceReader{ctx: ctx, file: f, remaining: remaining, hash: xxhash.New()}
	reader := bufio.NewReaderSize(r, 4096)
	var text strings.Builder
	reason := ""
	for line := 1; line <= end; {
		part, err := reader.ReadSlice('\n')
		if line >= start && len(part) > 0 {
			if source.StartLine == 0 {
				source.StartLine = line
			}
			n := min(len(part), evidenceExcerptLimit-text.Len())
			text.Write(part[:n])
			source.EndLine = line
			if n < len(part) || text.Len() == evidenceExcerptLimit {
				source.Truncated = true
				reason = "excerpt_limit"
				break
			}
		}
		if len(part) > 0 && part[len(part)-1] == '\n' {
			line++
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			if err == errEvidenceReadLimit {
				source.Truncated = true
				reason = "read_limit"
			} else if err != io.EOF {
				source.Truncated = true
				reason = "source_read_error"
			} else if line < end || (line == end && len(part) == 0) {
				source.Truncated = true
				reason = "range_unavailable"
			}
			break
		}
	}
	source.Text = text.String()
	if !utf8.ValidString(source.Text) {
		source.Text = strings.ToValidUTF8(source.Text, "�")
		source.Truncated = true
		reason = "non_utf8_source"
	}
	after, err := f.Stat()
	switch {
	case err != nil || before.Size() != after.Size() || before.ModTime() != after.ModTime():
		source.Freshness = "changed_during_read"
		reason = "changed_source"
	case source.IndexedHash == "":
		source.Freshness = "unindexed"
	case int64(r.n) == before.Size():
		if fmt.Sprintf("%016x", r.hash.Sum64()) == source.IndexedHash {
			source.Freshness = "verified"
		} else {
			source.Freshness = "stale"
			reason = "stale_source"
		}
	case indexedMtime != before.ModTime().Unix():
		source.Freshness = "stale_metadata"
		if reason == "" {
			reason = "unverified_source"
		}
	default:
		if reason == "" {
			reason = "unverified_source"
		}
	}
	evidenceExcerptHash(source)
	return source, reason, nil
}

func evidenceClip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func evidenceExcerptHash(source *evidenceSource) {
	if source.Text == "" {
		source.ExcerptSHA256 = ""
		source.StartLine = 0
		source.EndLine = 0
		return
	}
	hash := sha256.Sum256([]byte(source.Text))
	source.ExcerptSHA256 = hex.EncodeToString(hash[:])
	source.EndLine = source.StartLine + strings.Count(source.Text, "\n")
	if strings.HasSuffix(source.Text, "\n") {
		source.EndLine--
	}
}

// The complete serialized result is the only thing that certifies the budget:
// the fixed reserve plus 10% margin covers framing/tokenizer variation, not
// just source snippets, and all notices and provenance participate. Exact
// counts are expensive (BPE over the whole JSON), so between two exact checks
// the shrink steps are applied until the tokens they removed — counted on the
// removed JSON fragments only — cover the measured excess. The shrink order is
// unchanged: halve the last remaining excerpt, then drop trailing items, then
// compact omission counters.
func fitEvidence(ctx context.Context, result *evidenceResult, counter model.TokenCounter) error {
	// Shrinking empties excerpts back to front, so once the excerpts in front
	// already need more tokens than the whole budget, every later excerpt is
	// emptied before anything in front is touched. Skip tokenizing them.
	spent := 0
	for i := range result.Items {
		source := result.Items[i].Source
		if source == nil || source.Text == "" {
			continue
		}
		if spent > result.MaxTokens+64 {
			source.Text = ""
			source.Truncated = true
			evidenceExcerptHash(source)
			result.Truncated = true
			result.Omitted["output_budget"] = 1
			continue
		}
		spent += evidenceFragmentTokens(counter, source.Text)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("serialize graph evidence: %w", err)
		}
		// Every byte-level BPE token covers at least one byte, so the byte
		// length is an upper bound that accepts small results untokenized.
		if _, bpe := counter.(*model.BPECounter); bpe {
			if n := len(data); n+32+(n+9)/10 <= result.MaxTokens {
				return nil
			}
		}
		tokens := counter.CountString(string(data))
		excess := tokens + 32 + (tokens+9)/10 - result.MaxTokens
		if excess <= 0 {
			return nil
		}
		result.Truncated = true
		result.Omitted["output_budget"] = 1
		for removed := 0; removed < excess; {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Without items only envelope compaction remains; re-measure
			// before deciding the envelope itself cannot fit.
			if len(result.Items) == 0 && removed > 0 {
				break
			}
			gone, err := shrinkEvidence(result, counter)
			if err != nil {
				return err
			}
			removed += gone
		}
	}
}

// evidenceFragmentTokens counts the tokens of v's JSON encoding (at least 1).
func evidenceFragmentTokens(counter model.TokenCounter, v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 1
	}
	return max(1, counter.CountString(string(data)))
}

// shrinkEvidence applies one budget-reduction step and returns an estimate of
// the tokens it removed. It fails only when nothing else can be removed.
func shrinkEvidence(result *evidenceResult, counter model.TokenCounter) (int, error) {
	fragmentTokens := func(v any) int { return evidenceFragmentTokens(counter, v) }
	for i := len(result.Items) - 1; i >= 0; i-- {
		source := result.Items[i].Source
		if source == nil || source.Text == "" {
			continue
		}
		before := source.Text
		source.Text = evidenceClip(source.Text, len(source.Text)/2)
		if newline := strings.LastIndexByte(source.Text, '\n'); newline >= 0 {
			source.Text = source.Text[:newline+1]
		}
		source.Truncated = true
		evidenceExcerptHash(source)
		return fragmentTokens(before[len(source.Text):]), nil
	}
	if len(result.Items) > 0 {
		last := result.Items[len(result.Items)-1]
		result.Items = result.Items[:len(result.Items)-1]
		result.Omitted["items_budget"]++
		return fragmentTokens(last), nil
	}
	// At the minimum supported budget, collapse detailed omission counters
	// if the envelope itself is too large. The loss is explicit.
	if len(result.Omitted) > 2 {
		before := fragmentTokens(result.Omitted)
		count := 0
		for _, n := range result.Omitted {
			count += n
		}
		result.Omitted = map[string]int{"output_budget": 1, "omissions_compacted": count}
		return max(1, before-fragmentTokens(result.Omitted)), nil
	}
	return 0, fmt.Errorf("codebase_context: budget cannot hold result envelope")
}
