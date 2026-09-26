// Package graph exposes the chronos indexer (internal/indexer) to agents as
// zero-LLM-cost (T0) tools for structural code navigation: symbol lookup,
// callers and callees, implementations, impact analysis and graph-based
// codebase context.
package graph

import "github.com/spawn08/chronos-code/internal/indexer/facts"

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
	Exported  bool // visible outside its package or module
	Test      bool // a test function or method, in any language
	TestFile  bool // declared in a test file
}

// Qualified returns Recv.Name for methods and Name otherwise, the identity
// callers and callees are reported under.
func (s Symbol) Qualified() string {
	if s.Receiver == "" {
		return s.Name
	}
	return facts.BaseType(s.Receiver) + "." + s.Name
}

// CallEdge is a resolved call from one declaration to another: its first
// call site with the strongest resolution label.
type CallEdge struct {
	Caller, Callee Symbol
	Line           int    // call site line in Caller.File
	Resolution     string // import_resolved, type_hinted, name_matched, ambiguous, or unresolved (no indexed declaration)
	Candidates     int    // declarations the call site could equally target (>= 1)
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
