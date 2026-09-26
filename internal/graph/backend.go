package graph

import "context"

// Backend is what the graph tools query. The chronos indexer implements it
// over one immutable snapshot (see index_backend.go); tests may use fakes.
//
// Paths: FindSymbols and friends return Symbol.File root-relative; methods
// taking a path accept an absolute or root-relative path.
type Backend interface {
	FindSymbols(ctx context.Context, name, kind string) ([]Symbol, error)
	FindSymbolsFuzzy(ctx context.Context, substr string) ([]Symbol, error)
	Search(ctx context.Context, query string, topK int) ([]SearchResult, error)
	SymbolsInPackage(ctx context.Context, pkg string) ([]Symbol, error)
	FilesInPackage(ctx context.Context, pkg string) ([]FileRecord, error)
	SymbolsInFile(ctx context.Context, file string) ([]Symbol, error)
	// CallerEdges returns the resolved calls into every declaration named
	// name (as FindSymbols matches it); when none is indexed, the call sites
	// of that callee name, labelled unresolved.
	CallerEdges(ctx context.Context, name string) ([]CallEdge, error)
	// IncomingCalls returns the resolved calls into targets (declarations
	// from this backend), ordered by callee then caller file and line.
	IncomingCalls(ctx context.Context, targets []Symbol) ([]CallEdge, error)
	// CallersOf and CallersOfMany return the qualified identities of the
	// declarations CallerEdges finds.
	CallersOf(ctx context.Context, name string) ([]string, error)
	CallersOfMany(ctx context.Context, names []string) (map[string][]string, error)
	// CallerSymbols returns up to limit declarations calling the short
	// callee name, ordered by file, line and name.
	CallerSymbols(ctx context.Context, name string, limit int) ([]Symbol, error)
	SymbolsByQualified(ctx context.Context, names []string) (map[string][]Symbol, error)
	// CalleesOf returns the qualified identities of the workspace
	// declarations called by the declarations with qualified identity name.
	CalleesOf(ctx context.Context, name string) ([]string, error)
	ImplementationsOf(ctx context.Context, interfaceName string) ([]string, error)
	ImportsOf(ctx context.Context, pkg string) ([]string, error)
	ImportersOf(ctx context.Context, pkg string) ([]string, error)
	PackageImports(ctx context.Context, pkg string) (string, error)
	Packages(ctx context.Context) ([]string, error)
	Stats(ctx context.Context) (Stats, error)
	FileHash(ctx context.Context, path string) (string, error)
	// FileProvenance returns the indexed content hash and mtime (Unix
	// seconds) of the first of paths (in sorted order) that is indexed.
	FileProvenance(ctx context.Context, paths ...string) (hash string, mtime int64, err error)
}

// IndexReport describes how current a backend's answers are and how its
// relationships were matched. Backends that implement Reporter attach it to
// tool results, so an agent can trust an empty answer instead of re-checking.
type IndexReport struct {
	Mode       string // "syntactic": parsed, not type-checked
	Generation uint64
	Files      int
	UpToDate   bool
	Pending    int    // changed paths not yet indexed
	Building   bool   // the first build is still running
	Partial    bool   // only the working set is indexed so far (progressive first build)
	Relations  string // weakest matching behind implementations, e.g. "name_matched"; call edges carry their own label
	Error      string // last indexing error, if any
	// Repos are the federated repositories answering with the primary
	// workspace (workspace.indexer.federation), if any.
	Repos []RepoReport
}

// Reporter is implemented by backends that can describe their freshness.
type Reporter interface {
	IndexReport() IndexReport
}
