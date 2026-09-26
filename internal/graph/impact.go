package graph

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spawn08/chronos/engine/tool"

	"github.com/spawn08/chronos-code/indexer/scan"
)

// gitLogTimeout bounds how long co_change waits for `git log` before giving
// up (non-fatal — co_change degrades to an empty result outside a git repo
// or if git is slow/unavailable).
const gitLogTimeout = 10 * time.Second

// ImpactTools returns the three T0 tools added by PRD P2-011: impact_analysis
// (blast radius before an edit), test_map (find tests covering a symbol or
// file), and co_change (git-history co-change analysis). root is the
// workspace/git root used to resolve file paths and run git.
func ImpactTools(store Backend, root string) []*tool.Definition {
	return []*tool.Definition{
		impactAnalysisTool(store),
		testMapTool(store),
		coChangeTool(root),
	}
}

func impactAnalysisTool(store Backend) *tool.Definition {
	return &tool.Definition{
		Name:        "impact_analysis",
		Effects:     []tool.Effect{tool.EffectRead},
		Description: "Before editing code, compute blast radius: callers, affected tests, and a breaking-change heuristic for symbols declared in a file's line range.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file":       map[string]any{"type": "string", "description": "File path"},
				"start_line": map[string]any{"type": "integer", "description": "Start of edit range"},
				"end_line":   map[string]any{"type": "integer", "description": "End of edit range"},
			},
			"required": []string{"file", "start_line", "end_line"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			file, _ := args["file"].(string)
			if file == "" {
				return nil, fmt.Errorf("impact_analysis: file is required")
			}
			startLine := intArg(args["start_line"], 0)
			endLine := intArg(args["end_line"], 0)
			if endLine < startLine {
				endLine = startLine
			}

			symbols, err := store.SymbolsInFile(ctx, file)
			if err != nil {
				return nil, err
			}

			var affected []map[string]any
			breaking := false
			edges := newEdgeMemo(store)
			for _, sym := range symbols {
				if !rangesOverlap(sym.Line, sym.EndLine, startLine, endLine) {
					continue
				}
				in, err := edges.incoming(ctx, sym)
				if err != nil {
					return nil, err
				}
				tests := testsFor(ctx, edges, []Symbol{sym}, 3)

				var callers []string
				seen := map[string]bool{}
				externalCaller := false
				for _, e := range in {
					if e.Caller.Package != sym.Package {
						externalCaller = true
					}
					if q := repoQualified(e.Caller); !seen[q] {
						seen[q] = true
						callers = append(callers, q)
					}
				}
				sort.Strings(callers)
				if sym.Exported && externalCaller {
					breaking = true
				}

				affected = append(affected, map[string]any{
					"symbol":       sym.Name,
					"kind":         string(sym.Kind),
					"exported":     sym.Exported,
					"caller_count": len(callers),
					"callers":      callers,
					"tests":        tests,
				})
			}

			return map[string]any{
				"file":                      file,
				"start_line":                startLine,
				"end_line":                  endLine,
				"affected_symbols":          affected,
				"potential_breaking_change": breaking,
			}, nil
		},
	}
}

func testMapTool(store Backend) *tool.Definition {
	return &tool.Definition{
		Name:        "test_map",
		Effects:     []tool.Effect{tool.EffectRead},
		Description: "Find which tests exercise a function or file.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"symbol": map[string]any{"type": "string", "description": "Function name or file path"},
			},
			"required": []string{"symbol"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			symbol, _ := args["symbol"].(string)
			if symbol == "" {
				return nil, fmt.Errorf("test_map: symbol is required")
			}

			var syms []Symbol
			var err error
			if strings.Contains(symbol, "/") || scan.Indexable(symbol) {
				syms, err = store.SymbolsInFile(ctx, symbol)
			} else {
				syms, err = store.FindSymbols(ctx, symbol, "")
			}
			if err != nil {
				return nil, err
			}
			return map[string]any{"target": symbol, "tests": testsFor(ctx, newEdgeMemo(store), syms, 3)}, nil
		},
	}
}

func coChangeTool(root string) *tool.Definition {
	return &tool.Definition{
		Name:        "co_change",
		Effects:     []tool.Effect{tool.EffectRead},
		Description: "Find files that historically change together (from git log).",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file": map[string]any{"type": "string", "description": "File path"},
				"days": map[string]any{"type": "integer", "description": "Lookback window in days", "default": 90},
			},
			"required": []string{"file"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			file, _ := args["file"].(string)
			if file == "" {
				return nil, fmt.Errorf("co_change: file is required")
			}
			days := intArg(args["days"], 90)
			if days <= 0 {
				days = 90
			}

			related, err := coChangedFiles(root, file, days)
			if err != nil {
				// Non-fatal: no git repo, git unavailable, or file has no
				// history. Report an empty result rather than an error.
				return map[string]any{"file": file, "days": days, "co_changed": []map[string]any{}}, nil
			}
			return map[string]any{"file": file, "days": days, "co_changed": related}, nil
		},
	}
}

// testsFor walks the resolved callers of targets up to depth levels and
// returns the sorted qualified identities of the test functions among them
// (Symbol.Test: Go Test*/Benchmark*/Example*/Fuzz* in _test.go files, and
// each language pack's test names and markers). Tests are not expanded
// further.
func testsFor(ctx context.Context, edges *edgeMemo, targets []Symbol, depth int) []string {
	seen := map[int64]bool{}
	for _, t := range targets {
		seen[t.ID] = true
	}
	found := map[string]bool{}
	frontier := targets
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []Symbol
		for _, e := range edges.incomingAll(ctx, frontier) {
			if seen[e.Caller.ID] {
				continue
			}
			seen[e.Caller.ID] = true
			if e.Caller.Test {
				found[repoQualified(e.Caller)] = true
				continue
			}
			next = append(next, e.Caller)
		}
		frontier = next
	}
	tests := make([]string, 0, len(found))
	for t := range found {
		tests = append(tests, t)
	}
	sort.Strings(tests)
	return tests
}

// edgeMemo caches the incoming calls of each declaration for one tool
// call, since the callers of neighbouring symbols overlap.
type edgeMemo struct {
	store Backend
	by    map[int64][]CallEdge
}

func newEdgeMemo(store Backend) *edgeMemo {
	return &edgeMemo{store: store, by: map[int64][]CallEdge{}}
}

func (m *edgeMemo) incoming(ctx context.Context, s Symbol) ([]CallEdge, error) {
	if e, ok := m.by[s.ID]; ok {
		return e, nil
	}
	e, err := m.store.IncomingCalls(ctx, []Symbol{s})
	if err != nil {
		return nil, err
	}
	m.by[s.ID] = e
	return e, nil
}

// incomingAll returns the incoming calls of every symbol; symbols whose
// lookup fails contribute none.
func (m *edgeMemo) incomingAll(ctx context.Context, syms []Symbol) []CallEdge {
	var out []CallEdge
	for _, s := range syms {
		if e, err := m.incoming(ctx, s); err == nil {
			out = append(out, e...)
		}
	}
	return out
}

func coChangedFiles(root, file string, days int) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitLogTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", root, "log",
		fmt.Sprintf("--since=%d.days", days),
		"--name-only", "--pretty=format:__COMMIT__", "--", file)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	counts := map[string]int{}
	lines := strings.Split(stdout.String(), "\n")
	inCommit := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case line == "__COMMIT__":
			inCommit = true
		case line == "":
			inCommit = false
		case inCommit:
			if line != file {
				counts[line]++
			}
		}
	}

	type pair struct {
		file  string
		count int
	}
	pairs := make([]pair, 0, len(counts))
	for f, c := range counts {
		pairs = append(pairs, pair{f, c})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].file < pairs[j].file
	})
	if len(pairs) > 20 {
		pairs = pairs[:20]
	}

	out := make([]map[string]any, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, map[string]any{"file": p.file, "co_change_count": p.count})
	}
	return out, nil
}

// rangesOverlap reports whether [aStart,aEnd] and [bStart,bEnd] intersect.
func rangesOverlap(aStart, aEnd, bStart, bEnd int) bool {
	if aEnd < aStart {
		aEnd = aStart
	}
	return aStart <= bEnd && bStart <= aEnd
}
