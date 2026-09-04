package graph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spawn08/chronos/engine/tool"
)

const (
	codebaseMapMaxOutputBytes   = 64 * 1024
	codebaseMapTruncationNotice = "\n\n_Output truncated at 65536 bytes._\n"
)

// Tools returns the T0 (zero-LLM-cost) graph navigation tools. root is the
// workspace root, used to resolve paths for source snippets and code maps.
func Tools(store *Store, root string) []*tool.Definition {
	return []*tool.Definition{
		graphQueryTool(store),
		codebaseSearchTool(store),
		codebaseMapTool(store, root),
		findCallersTool(store),
		findImplementationsTool(store),
		multiResolutionViewTool(store, root),
		resolveSymbolTool(store),
	}
}

func codebaseMapTool(store *Store, root string) *tool.Definition {
	return &tool.Definition{
		Name:        "codebase_map",
		Description: "Render deterministic package maps, selecting packages by indexed symbol relevance and package name.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Optional symbol or package query; omit for the package index"},
			},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			query, _ := args["query"].(string)
			query = strings.TrimSpace(query)
			if query == "" {
				index, err := RenderCodeMapIndex(ctx, store)
				if err != nil {
					return nil, err
				}
				return limitCodebaseMapOutputBytes(index), nil
			}

			results, err := store.Search(ctx, query, 100)
			if err != nil {
				return nil, err
			}
			packages := make([]string, 0, len(results))
			seen := make(map[string]struct{}, len(results))
			for _, result := range results {
				if _, ok := seen[result.Package]; ok {
					continue
				}
				seen[result.Package] = struct{}{}
				packages = append(packages, result.Package)
			}

			allPackages, err := store.Packages(ctx)
			if err != nil {
				return nil, err
			}
			foldedQuery := strings.ToLower(query)
			for _, pkg := range allPackages {
				if !strings.Contains(strings.ToLower(pkg), foldedQuery) {
					continue
				}
				if _, ok := seen[pkg]; ok {
					continue
				}
				seen[pkg] = struct{}{}
				packages = append(packages, pkg)
			}

			var output strings.Builder
			for i, pkg := range packages {
				codeMap, err := RenderCodeMap(ctx, store, root, pkg)
				if err != nil {
					return nil, err
				}
				if i > 0 {
					output.WriteByte('\n')
				}
				output.WriteString(codeMap)
			}
			return limitCodebaseMapOutputBytes(output.String()), nil
		},
	}
}

// limitCodebaseMapOutputBytes caps tool output in bytes without splitting a
// UTF-8 encoding. The truncation notice is included within the byte limit.
func limitCodebaseMapOutputBytes(output string) string {
	if len(output) <= codebaseMapMaxOutputBytes {
		return output
	}
	end := codebaseMapMaxOutputBytes - len(codebaseMapTruncationNotice)
	for end > 0 && !utf8.ValidString(output[:end]) {
		end--
	}
	return output[:end] + codebaseMapTruncationNotice
}

func codebaseSearchTool(store *Store) *tool.Definition {
	return &tool.Definition{
		Name:        "codebase_search",
		Description: "Search indexed symbols by exact name and full-text relevance.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Symbol name or full-text search query"},
				"top_k": map[string]any{"type": "integer", "description": "Maximum FTS results (default 10, max 100)"},
			},
			"required": []string{"query"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			query, _ := args["query"].(string)
			query = strings.TrimSpace(query)
			if query == "" {
				return nil, fmt.Errorf("codebase_search: query is required")
			}

			exact, err := store.FindSymbols(ctx, query, "")
			if err != nil {
				return nil, err
			}
			results, err := store.Search(ctx, query, intArg(args["top_k"], 10))
			if err != nil {
				return nil, err
			}

			syms := make([]Symbol, 0, len(exact)+len(results))
			seen := make(map[int64]struct{}, len(exact)+len(results))
			for _, sym := range exact {
				seen[sym.ID] = struct{}{}
				syms = append(syms, sym)
			}
			for _, result := range results {
				if _, ok := seen[result.ID]; ok {
					continue
				}
				seen[result.ID] = struct{}{}
				syms = append(syms, result.Symbol)
			}
			return map[string]any{"found": len(syms) > 0, "symbols": symbolSummaries(syms)}, nil
		},
	}
}

func graphQueryTool(store *Store) *tool.Definition {
	return &tool.Definition{
		Name:        "graph_query",
		Description: "Look up a symbol by name. Returns kind, location, signature, and doc for every match.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Symbol name (exact match)"},
				"kind": map[string]any{"type": "string", "description": "Optional filter: func, method, type, interface, struct, var, const"},
			},
			"required": []string{"name"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			name, _ := args["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("graph_query: name is required")
			}
			kind, _ := args["kind"].(string)
			syms, err := store.FindSymbols(ctx, name, kind)
			if err != nil {
				return nil, err
			}
			if len(syms) == 0 {
				fuzzy, ferr := store.FindSymbolsFuzzy(ctx, name)
				if ferr == nil && len(fuzzy) > 0 {
					return map[string]any{"found": false, "did_you_mean": symbolSummaries(fuzzy)}, nil
				}
				return map[string]any{"found": false}, nil
			}
			return map[string]any{"found": true, "symbols": symbolSummaries(syms)}, nil
		},
	}
}

func findCallersTool(store *Store) *tool.Definition {
	return &tool.Definition{
		Name:        "find_callers",
		Description: "Find functions that call a given function or method, up to a bounded call-chain depth.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":  map[string]any{"type": "string", "description": "Function or method name"},
				"depth": map[string]any{"type": "integer", "description": "Call chain depth (default 1, max 3)"},
			},
			"required": []string{"name"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			name, _ := args["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("find_callers: name is required")
			}
			depth := intArg(args["depth"], 1)
			if depth < 1 {
				depth = 1
			}
			if depth > 3 {
				depth = 3
			}
			frontier := []string{name}
			seen := map[string]bool{name: true}
			levels := make([]map[string][]string, 0, depth)
			for d := 0; d < depth; d++ {
				level := make(map[string][]string)
				var next []string
				for _, n := range frontier {
					callers, err := store.CallersOf(ctx, n)
					if err != nil {
						return nil, err
					}
					level[n] = callers
					for _, c := range callers {
						if !seen[c] {
							seen[c] = true
							next = append(next, c)
						}
					}
				}
				levels = append(levels, level)
				if len(next) == 0 {
					break
				}
				frontier = next
			}
			return map[string]any{"name": name, "callers_by_depth": levels}, nil
		},
	}
}

func findImplementationsTool(store *Store) *tool.Definition {
	return &tool.Definition{
		Name:        "find_implementations",
		Description: "Find concrete types that implement a given interface.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Interface name"},
			},
			"required": []string{"name"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			name, _ := args["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("find_implementations: name is required")
			}
			impls, err := store.ImplementationsOf(ctx, name)
			if err != nil {
				return nil, err
			}
			return map[string]any{"interface": name, "implementations": impls}, nil
		},
	}
}

func resolveSymbolTool(store *Store) *tool.Definition {
	return &tool.Definition{
		Name:        "resolve_symbol",
		Description: "Go-to-definition: resolve a symbol name to its definition location(s).",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":         map[string]any{"type": "string", "description": "Symbol name"},
				"context_file": map[string]any{"type": "string", "description": "File referencing the symbol, for disambiguation when multiple matches exist"},
			},
			"required": []string{"name"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			name, _ := args["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("resolve_symbol: name is required")
			}
			syms, err := store.FindSymbols(ctx, name, "")
			if err != nil {
				return nil, err
			}
			if len(syms) == 0 {
				return map[string]any{"found": false}, nil
			}
			if len(syms) > 1 {
				if ctxFile, _ := args["context_file"].(string); ctxFile != "" {
					ctxDir := filepath.Dir(ctxFile)
					for _, s := range syms {
						if filepath.Dir(s.File) == ctxDir {
							return map[string]any{"found": true, "symbol": symbolSummary(s)}, nil
						}
					}
				}
				return map[string]any{"found": true, "ambiguous": true, "candidates": symbolSummaries(syms)}, nil
			}
			return map[string]any{"found": true, "symbol": symbolSummary(syms[0])}, nil
		},
	}
}

func multiResolutionViewTool(store *Store, root string) *tool.Definition {
	return &tool.Definition{
		Name:        "multi_resolution_view",
		Description: "View code at a chosen zoom level. L0=repo overview, L1=package summary, L2=symbol summary, L3=source snippet.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{"type": "string", "description": "Package name, file path, or symbol name (ignored for L0)"},
				"level":  map[string]any{"type": "string", "description": "L0, L1, L2, or L3"},
			},
			"required": []string{"target", "level"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			level, _ := args["level"].(string)
			target, _ := args["target"].(string)
			switch strings.ToUpper(level) {
			case "L0":
				pkgs, err := store.Packages(ctx)
				if err != nil {
					return nil, err
				}
				stats, err := store.Stats(ctx)
				if err != nil {
					return nil, err
				}
				return map[string]any{"level": "L0", "packages": pkgs, "stats": stats}, nil
			case "L1":
				if target == "" {
					return nil, fmt.Errorf("multi_resolution_view: target is required for L1")
				}
				syms, err := store.SymbolsInPackage(ctx, target)
				if err != nil {
					return nil, err
				}
				imports, err := store.PackageImports(ctx, target)
				if err != nil {
					return nil, err
				}
				var importList []string
				if imports != "" {
					importList = strings.Split(imports, ",")
				}
				return map[string]any{"level": "L1", "package": target, "imports": importList, "symbols": symbolSummaries(syms)}, nil
			case "L2":
				if target == "" {
					return nil, fmt.Errorf("multi_resolution_view: target is required for L2")
				}
				syms, err := store.FindSymbols(ctx, target, "")
				if err != nil {
					return nil, err
				}
				if len(syms) == 0 {
					return map[string]any{"level": "L2", "found": false}, nil
				}
				out := make([]map[string]any, 0, len(syms))
				for _, s := range syms {
					callers, _ := store.CallersOf(ctx, s.Name)
					callees, _ := store.CalleesOf(ctx, s.Name)
					sum := symbolSummary(s)
					sum["caller_count"] = len(callers)
					sum["callee_count"] = len(callees)
					out = append(out, sum)
				}
				return map[string]any{"level": "L2", "symbols": out}, nil
			case "L3":
				if target == "" {
					return nil, fmt.Errorf("multi_resolution_view: target is required for L3")
				}
				return l3Snippet(ctx, store, root, target)
			default:
				return nil, fmt.Errorf("multi_resolution_view: unknown level %q (want L0, L1, L2, L3)", level)
			}
		},
	}
}

// l3Snippet resolves target (a symbol name or file path) to source text.
func l3Snippet(ctx context.Context, store *Store, root, target string) (any, error) {
	path := target
	startLine, endLine := 0, 0
	if !strings.Contains(target, string(os.PathSeparator)) && !strings.HasSuffix(target, ".go") {
		syms, err := store.FindSymbols(ctx, target, "")
		if err != nil {
			return nil, err
		}
		if len(syms) == 0 {
			return map[string]any{"level": "L3", "found": false}, nil
		}
		path = syms[0].File
		startLine, endLine = syms[0].Line, syms[0].EndLine
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	var err error
	path, err = l3PathWithinRoot(root, path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if startLine == 0 {
		return map[string]any{"level": "L3", "found": true, "file": target, "content": string(data)}, nil
	}
	lines := strings.Split(string(data), "\n")
	if startLine < 1 {
		startLine = 1
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	snippet := strings.Join(lines[startLine-1:endLine], "\n")
	return map[string]any{"level": "L3", "found": true, "file": target, "start_line": startLine, "end_line": endLine, "content": snippet}, nil
}

// l3PathWithinRoot rejects lexical and symlink escapes before L3 reads source.
func l3PathWithinRoot(root, path string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("multi_resolution_view: workspace root is required for L3")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("multi_resolution_view: resolve workspace root: %w", err)
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("multi_resolution_view: resolve L3 path: %w", err)
	}
	if !isPathWithin(rootAbs, pathAbs) {
		return "", fmt.Errorf("multi_resolution_view: L3 target %q is outside the workspace root", path)
	}

	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("multi_resolution_view: resolve workspace root: %w", err)
	}
	pathReal, err := filepath.EvalSymlinks(pathAbs)
	if err != nil {
		return "", fmt.Errorf("multi_resolution_view: resolve L3 path: %w", err)
	}
	if !isPathWithin(rootReal, pathReal) {
		return "", fmt.Errorf("multi_resolution_view: L3 target %q resolves outside the workspace root", path)
	}
	return pathReal, nil
}

func isPathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))))
}

func symbolSummary(s Symbol) map[string]any {
	return map[string]any{
		"name":      s.Name,
		"kind":      string(s.Kind),
		"package":   s.Package,
		"file":      s.File,
		"line":      s.Line,
		"signature": s.Signature,
		"doc":       s.Doc,
		"receiver":  s.Receiver,
	}
}

func symbolSummaries(syms []Symbol) []map[string]any {
	out := make([]map[string]any, 0, len(syms))
	for _, s := range syms {
		out = append(out, symbolSummary(s))
	}
	return out
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
