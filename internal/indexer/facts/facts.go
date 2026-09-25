// Package facts defines the per-file records produced by extractors and stored
// in index segments. Every record is derived from a single parse of one file;
// nothing here depends on other files, so a file's facts never need rewriting
// when another file changes.
package facts

// Symbol kinds. Values match the existing graph tool contract.
const (
	KindFunc      = "func"
	KindMethod    = "method"
	KindType      = "type"
	KindInterface = "interface"
	KindStruct    = "struct"
	KindVar       = "var"
	KindConst     = "const"
)

// Kinds lists every symbol kind in its stable on-disk order.
var Kinds = []string{KindFunc, KindMethod, KindType, KindInterface, KindStruct, KindVar, KindConst}

// Qualifier kinds of a call site.
const (
	// QualNone is an unqualified call: f().
	QualNone uint8 = iota
	// QualPackage is pkg.F() where pkg is an import; Qualifier holds the import path.
	QualPackage
	// QualExpr is x.M() on a value; Qualifier holds the receiver expression text.
	QualExpr
)

// NoCaller marks a call outside any declaration (package-level initializers).
const NoCaller = -1

// File is everything indexed about one source file.
type File struct {
	Path     string // root-relative, slash-separated
	Lang     string
	Package  string // import path (Go) or directory
	PkgName  string // declared package name
	Hash     uint64 // xxh64 of the content
	Size     int64
	MtimeNS  int64
	ParseErr string
	Deleted  bool // tombstone: the path no longer exists or is no longer indexed

	Symbols []Symbol
	Imports []Import
	Calls   []Call
}

// Symbol is one declaration.
type Symbol struct {
	Name      string
	Kind      string
	Receiver  string // receiver type expression for methods, e.g. "*Engine"
	Signature string
	Doc       string
	Line      int
	EndLine   int
	Exported  bool
}

// Qualified returns Recv.Name for methods (receiver without '*' or type
// parameters) and Name otherwise.
func (s Symbol) Qualified() string {
	if s.Receiver == "" {
		return s.Name
	}
	return BaseType(s.Receiver) + "." + s.Name
}

// BaseType strips pointer and type-parameter syntax from a receiver type.
func BaseType(recv string) string {
	for len(recv) > 0 && recv[0] == '*' {
		recv = recv[1:]
	}
	for i := 0; i < len(recv); i++ {
		if recv[i] == '[' {
			return recv[:i]
		}
	}
	return recv
}

// Import is one import declaration.
type Import struct {
	Path string
	Name string // explicit local name ("" when absent, "_" or ".")
	Line int
}

// Call is one unresolved call site. Resolution happens at query time.
type Call struct {
	Caller    int // index into File.Symbols, or NoCaller
	Callee    string
	Qualifier string
	QualKind  uint8
	Line      int
	Col       int
}
