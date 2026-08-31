package graph

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/spawn08/chronos/engine/tool"
)

// gitLogTimeout bounds how long co_change waits for `git log` before giving
// up (non-fatal — co_change degrades to an empty result outside a git repo
// or if git is slow/unavailable).
const gitLogTimeout = 10 * time.Second

// ImpactTools returns the three T0 tools added by PRD P2-011: impact_analysis
// (blast radius before an edit), test_map (find tests covering a symbol or
// file), and co_change (git-history co-change analysis). root is the
// workspace/git root used to resolve file paths and run git.
func ImpactTools(store *Store, root string) []*tool.Definition {
	return []*tool.Definition{
		impactAnalysisTool(store),
		testMapTool(store),
		coChangeTool(root),
	}
}

func impactAnalysisTool(store *Store) *tool.Definition {
	return &tool.Definition{
		Name:        "impact_analysis",
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
			for _, sym := range symbols {
				if !rangesOverlap(sym.Line, sym.EndLine, startLine, endLine) {
					continue
				}
				callers, err := store.CallersOf(ctx, sym.Name)
				if err != nil {
					return nil, err
				}
				tests := testsForSymbol(ctx, store, sym.Name, 3)

				externalCaller := false
				for _, c := range callers {
					callerSyms, _ := store.FindSymbols(ctx, c, "")
					for _, cs := range callerSyms {
						if cs.Package != sym.Package {
							externalCaller = true
						}
					}
				}
				if isExported(sym.Name) && externalCaller {
					breaking = true
				}

				affected = append(affected, map[string]any{
					"symbol":       sym.Name,
					"kind":         string(sym.Kind),
					"exported":     isExported(sym.Name),
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

func testMapTool(store *Store) *tool.Definition {
	return &tool.Definition{
		Name:        "test_map",
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

			if strings.Contains(symbol, "/") || strings.HasSuffix(symbol, ".go") {
				syms, err := store.SymbolsInFile(ctx, symbol)
				if err != nil {
					return nil, err
				}
				seen := map[string]bool{}
				var tests []string
				for _, sym := range syms {
					for _, t := range testsForSymbol(ctx, store, sym.Name, 3) {
						if !seen[t] {
							seen[t] = true
							tests = append(tests, t)
						}
					}
				}
				sort.Strings(tests)
				return map[string]any{"target": symbol, "tests": tests}, nil
			}

			tests := testsForSymbol(ctx, store, symbol, 3)
			return map[string]any{"target": symbol, "tests": tests}, nil
		},
	}
}

func coChangeTool(root string) *tool.Definition {
	return &tool.Definition{
		Name:        "co_change",
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

// testsForSymbol walks the caller graph of name up to depth levels, keeping
// any caller that looks like a Go test function (name starts with "Test" and
// its declaring file ends with "_test.go" — the file is recovered via
// FindSymbols since CallersOf only returns names).
func testsForSymbol(ctx context.Context, store *Store, name string, depth int) []string {
	seen := map[string]bool{name: true}
	frontier := []string{name}
	var tests []string
	testSeen := map[string]bool{}

	for d := 0; d < depth; d++ {
		var next []string
		for _, n := range frontier {
			callers, err := store.CallersOf(ctx, n)
			if err != nil {
				continue
			}
			for _, c := range callers {
				if seen[c] {
					continue
				}
				seen[c] = true
				next = append(next, c)

				if strings.HasPrefix(c, "Test") {
					if syms, err := store.FindSymbols(ctx, c, ""); err == nil {
						for _, s := range syms {
							if strings.HasSuffix(s.File, "_test.go") && !testSeen[c] {
								testSeen[c] = true
								tests = append(tests, c)
							}
						}
					}
				}
			}
		}
		if len(next) == 0 {
			break
		}
		frontier = next
	}
	sort.Strings(tests)
	return tests
}

// coChangedFiles shells out to `git log` to find files that were modified in
// the same commits as file, within the last days days, ranked by frequency.
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

// isExported reports whether a Go identifier is exported (starts with an
// uppercase letter).
func isExported(name string) bool {
	if name == "" {
		return false
	}
	r := []rune(name)[0]
	return unicode.IsUpper(r)
}
