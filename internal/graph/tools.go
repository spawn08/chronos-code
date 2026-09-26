package graph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spawn08/chronos/engine/tool"

	"github.com/spawn08/chronos-code/indexer/query"
	"github.com/spawn08/chronos-code/indexer/scan"
)

const (
	codebaseMapMaxOutputBytes   = 64 * 1024
	codebaseMapTruncationNotice = "\n\n_Output truncated at 65536 bytes._\n"
)

// Tools returns the T0 (zero-LLM-cost) graph navigation tools. root is the
// workspace root, used to resolve paths for source snippets and code maps.
func Tools(store Backend, root string) []*tool.Definition {
	return []*tool.Definition{
		batched(graphQueryTool(store)),
		codebaseSearchTool(store),
		codebaseContextTool(store, root),
		codebaseMapTool(store, root),
		batched(findCallersTool(store)),
		batched(findImplementationsTool(store)),
		multiResolutionViewTool(store, root),
		batched(resolveSymbolTool(store)),
	}
}

func codebaseMapTool(store Backend, root string) *tool.Definition {
	return &tool.Definition{
		Name:        "codebase_map",
		Effects:     []tool.Effect{tool.EffectRead},
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

func codebaseSearchTool(store Backend) *tool.Definition {
	return &tool.Definition{
		Name:        "codebase_search",
		Effects:     []tool.Effect{tool.EffectRead},
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
			return labelResult(store, map[string]any{"found": len(syms) > 0, "symbols": symbolSummaries(syms)}, false, len(syms) == 0,
				fmt.Sprintf("No symbol matches %q by exact name or full-text search", query)), nil
		},
	}
}

func graphQueryTool(store Backend) *tool.Definition {
	return &tool.Definition{
		Name:        "graph_query",
		Effects:     []tool.Effect{tool.EffectRead},
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
				note := fmt.Sprintf("No declaration named %q: exact-name lookup over the whole index", name)
				fuzzy, ferr := store.FindSymbolsFuzzy(ctx, name)
				if ferr == nil && len(fuzzy) > 0 {
					return labelResult(store, map[string]any{"found": false, "did_you_mean": symbolSummaries(fuzzy)}, false, true, note), nil
				}
				return labelResult(store, map[string]any{"found": false}, false, true, note), nil
			}
			return labelResult(store, map[string]any{"found": true, "symbols": symbolSummaries(syms)}, false, false, ""), nil
		},
	}
}

func findCallersTool(store Backend) *tool.Definition {
	return &tool.Definition{
		Name:        "find_callers",
		Effects:     []tool.Effect{tool.EffectRead},
		Description: "Find functions that call a given function or method, up to a bounded call-chain depth. Callers are grouped by callee and by how the call was resolved (type_checked, import_resolved, type_hinted, name_matched, ambiguous; unresolved when the callee is not indexed), each as \"Caller (file:line)\". Ambiguous callers are not followed to the next depth.",
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
			edges, err := store.CallerEdges(ctx, name)
			if err != nil {
				return nil, err
			}
			// Each level keys callers by the callee's qualified identity, then
			// by resolution label; the next level asks for the callers of
			// those callers, by declaration. Ambiguous callers are listed but
			// not followed: their fan-out is mostly noise.
			seen := map[int64]bool{}
			levels := make([]map[string]map[string][]string, 0, depth)
			empty := true
			for d := 0; d < depth; d++ {
				level := map[string]map[string][]string{}
				var next []Symbol
				for _, e := range edges {
					if e.Callee.ID != 0 {
						seen[e.Callee.ID] = true
					}
					key := repoQualified(e.Callee)
					if level[key] == nil {
						level[key] = map[string][]string{}
					}
					level[key][e.Resolution] = append(level[key][e.Resolution], callerEntry(e))
					empty = false
					if !seen[e.Caller.ID] && e.Resolution != query.Ambiguous {
						seen[e.Caller.ID] = true
						next = append(next, e.Caller)
					}
				}
				if len(level) == 0 && d == 0 {
					level[name] = map[string][]string{}
				}
				levels = append(levels, level)
				if len(next) == 0 || d+1 == depth {
					break
				}
				if edges, err = store.IncomingCalls(ctx, next); err != nil {
					return nil, err
				}
			}
			return labelResult(store, map[string]any{"name": name, "callers_by_depth": levels}, false, empty,
				fmt.Sprintf("No callers of %q: no indexed call site resolves to a declaration with that name; calls through function values or reflection are not visible", name)), nil
		},
	}
}

// callerEntry renders one caller as "Caller (file:line)", with the number
// of declarations an ambiguous call site could target.
func callerEntry(e CallEdge) string {
	var extra string
	if e.Candidates > 1 {
		extra = fmt.Sprintf(", 1 of %d candidates", e.Candidates)
	}
	if e.Caller.Repo != "" || e.Via != "" {
		extra += ", " + repoLabel(e.Caller.Repo)
		if e.Via != "" {
			extra += " via " + e.Via
		}
	}
	return fmt.Sprintf("%s (%s:%d%s)", e.Caller.Qualified(), e.Caller.File, e.Line, extra)
}

// repoLabel names a federated repository in tool output; "" is the
// primary workspace.
func repoLabel(repo string) string {
	if repo == "" {
		return "this workspace"
	}
	return "repo " + repo
}

// repoQualified is a symbol's qualified name, prefixed "repo:" when a
// federated repository declares it.
func repoQualified(s Symbol) string {
	if s.Repo != "" {
		return s.Repo + ":" + s.Qualified()
	}
	return s.Qualified()
}

func findImplementationsTool(store Backend) *tool.Definition {
	return &tool.Definition{
		Name:        "find_implementations",
		Effects:     []tool.Effect{tool.EffectRead},
		Description: "Find concrete types that implement a given interface or extend a given type.",
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
			note := fmt.Sprintf("No indexed type implements %q (Go: a type declaring every method of the interface, matched by method name; other languages: a type whose extends or implements clause resolves to it)", name)
			if len(impls) == 0 {
				if _, reports := store.(Reporter); reports {
					if ifaces, err := store.FindSymbols(ctx, name, string(KindInterface)); err == nil && len(ifaces) == 0 {
						note = fmt.Sprintf("No interface named %q is indexed", name)
					}
				}
			}
			return labelResult(store, map[string]any{"interface": name, "implementations": impls}, true, len(impls) == 0, note), nil
		},
	}
}

func resolveSymbolTool(store Backend) *tool.Definition {
	return &tool.Definition{
		Name:        "resolve_symbol",
		Effects:     []tool.Effect{tool.EffectRead},
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
				return labelResult(store, map[string]any{"found": false}, false, true,
					fmt.Sprintf("No declaration named %q: exact-name lookup over the whole index", name)), nil
			}
			if len(syms) > 1 {
				if ctxFile, _ := args["context_file"].(string); ctxFile != "" {
					ctxDir := filepath.Dir(ctxFile)
					for _, s := range syms {
						if filepath.Dir(s.File) == ctxDir {
							return labelResult(store, map[string]any{"found": true, "symbol": symbolSummary(s)}, false, false, ""), nil
						}
					}
				}
				return labelResult(store, map[string]any{"found": true, "ambiguous": true, "candidates": symbolSummaries(syms)}, false, false, ""), nil
			}
			return labelResult(store, map[string]any{"found": true, "symbol": symbolSummary(syms[0])}, false, false, ""), nil
		},
	}
}

func multiResolutionViewTool(store Backend, root string) *tool.Definition {
	return &tool.Definition{
		Name:        "multi_resolution_view",
		Effects:     []tool.Effect{tool.EffectRead},
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
				dependsOn, err := store.ImportsOf(ctx, target)
				if err != nil {
					return nil, err
				}
				dependents, err := store.ImportersOf(ctx, target)
				if err != nil {
					return nil, err
				}
				return map[string]any{"level": "L1", "package": target, "imports": importList, "depends_on": dependsOn, "dependents": dependents, "symbols": symbolSummaries(syms)}, nil
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
					edges, _ := store.IncomingCalls(ctx, []Symbol{s})
					callers := map[int64]bool{}
					for _, e := range edges {
						callers[e.Caller.ID] = true
					}
					callees, _ := store.CalleesOf(ctx, s.Qualified())
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
func l3Snippet(ctx context.Context, store Backend, root, target string) (any, error) {
	path := target
	startLine, endLine := 0, 0
	if !strings.Contains(target, string(os.PathSeparator)) && !scan.Indexable(target) {
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
	out := map[string]any{
		"name":      s.Name,
		"kind":      string(s.Kind),
		"package":   s.Package,
		"file":      s.File,
		"line":      s.Line,
		"signature": s.Signature,
		"doc":       s.Doc,
		"receiver":  s.Receiver,
	}
	if s.Repo != "" {
		out["repo"] = s.Repo
	}
	return out
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
