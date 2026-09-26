// Package graph exposes the chronos indexer (internal/indexer) to agents as
// zero-LLM-cost (T0) tools for structural code navigation: symbol lookup,
// callers and callees, implementations, impact analysis and graph-based
// codebase context.
package graph

import "strings"

// SymbolKind classifies a graph symbol.
type SymbolKind string

const (
	KindFunc      SymbolKind = "func"
	KindMethod    SymbolKind = "method"
	KindType      SymbolKind = "type"
	KindInterface SymbolKind = "interface"
	KindStruct    SymbolKind = "struct"
	KindVar       SymbolKind = "var"
	KindConst     SymbolKind = "const"
)

// Symbol is a single declaration recorded in the graph.
type Symbol struct {
	ID        int64
	Name      string
	Kind      SymbolKind
	Package   string
	File      string
	Line      int
	EndLine   int
	Signature string
	Doc       string
	Receiver  string
}

// FileRecord is file-level metadata recorded in the graph.
type FileRecord struct {
	Path    string
	Package string
}

// SearchResult is a symbol matched by text search with its rank.
type SearchResult struct {
	Symbol
	Rank float64
}

// Stats reports the current size of the graph.
type Stats struct {
	Files    int
	Packages int
	Symbols  int
	Edges    int
}

// callTarget maps a caller identity ("Recv.Method" or "Func") to the short
// name its own callers are recorded under, for the next hop of a caller
// traversal.
func callTarget(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}
