// Package facts defines the per-file records produced by extractors and stored
// in index segments. Every record is derived from a single parse of one file;
// nothing here depends on other files, so a file's facts never need rewriting
// when another file changes.
//
// The records are language-neutral (facts v2). Go-specific meaning is noted
// where a field is interpreted differently for Go.
package facts

// Symbol kinds. The first eight match the existing graph tool contract.
const (
	KindFunc      = "func"
	KindMethod    = "method"
	KindType      = "type"
	KindInterface = "interface"
	KindStruct    = "struct"
	KindVar       = "var"
	KindConst     = "const"
	// KindEmbed is a type embedded in an interface or struct. Its Name is the
	// embedded type's base name and its Signature the full type expression
	// (e.g. "io.Reader"). Embeds are structure, not declarations: symbol
	// lookups skip them.
	KindEmbed = "embed"

	KindClass       = "class"
	KindConstructor = "constructor"
	KindTrait       = "trait"
	KindProtocol    = "protocol"
	KindEnum        = "enum"
	KindEnumMember  = "enum_member"
	KindField       = "field"
	KindProperty    = "property"
	KindTypeAlias   = "type_alias"
	KindModule      = "module"
	KindNamespace   = "namespace"
	KindMacro       = "macro"
)

// Kinds lists every symbol kind in its stable on-disk order. Append only.
var Kinds = []string{
	KindFunc, KindMethod, KindType, KindInterface, KindStruct, KindVar, KindConst, KindEmbed,
	KindClass, KindConstructor, KindTrait, KindProtocol, KindEnum, KindEnumMember,
	KindField, KindProperty, KindTypeAlias, KindModule, KindNamespace, KindMacro,
}

// Visibility of a symbol. Values are stored on disk; append only.
const (
	VisUnknown uint8 = iota
	VisPublic
	VisProtected
	VisInternal // C# internal, Kotlin internal, Swift internal
	VisPrivate
	VisPackage // Go unexported, Java package-private
	NumVisibility
)

// Symbol modifiers, a bitset. Bits are stored on disk; append only.
const (
	ModStatic uint32 = 1 << iota
	ModAbstract
	ModAsync
	ModOverride
	ModDeprecated
	ModTest
	// ModDecl marks a declaration without a definition (a C/C++ prototype
	// in a header, an abstract or interface member).
	ModDecl
)

// Qualifier kinds of a reference.
const (
	// QualNone is an unqualified reference: f().
	QualNone uint8 = iota
	// QualPackage is pkg.F() where pkg names an import; Qualifier holds the
	// import spec (the Go import path).
	QualPackage
	// QualExpr is x.M() on a value; Qualifier holds the receiver expression text.
	QualExpr
)

// Reference kinds. Values are stored on disk; append only.
const (
	RefCall        uint8 = iota // f(), x.m()
	RefTypeUse                  // a type named in a signature, declaration or expression
	RefExtends                  // superclass, embedded or extended type
	RefImplements               // implemented interface, protocol or trait
	RefInstantiate              // new T(), T{}, T()
	RefDecorator                // @decorator, [Attribute], @Annotation
	NumRefKinds
)

// Import kinds. Values are stored on disk; append only.
const (
	ImportModule   uint8 = iota // import x, import x as y, from x import a
	ImportWildcard              // from x import *, import static a.*, Go dot import
	ImportReexport              // export … from x (the import side of a re-export)
	ImportInclude               // #include, #import
	NumImportKinds
)

// NoCaller marks a reference outside any declaration (package-level
// initializers, top-level statements).
const NoCaller = -1

// File is everything indexed about one source file.
type File struct {
	Path string // root-relative, slash-separated
	Lang string
	// Package is the file's unit: the Go import path; for other languages
	// the directory until language resolvers (M7) assign modules, crates or
	// build targets.
	Package  string
	PkgName  string // declared package or namespace name, if any
	Hash     uint64 // xxh64 of the content
	Size     int64
	MtimeNS  int64
	ParseErr string
	Deleted  bool // tombstone: the path no longer exists or is no longer indexed

	Generated bool // generated code (e.g. a "Code generated … DO NOT EDIT." header)
	Vendored  bool // third-party code checked into the repository
	Test      bool // a test file by the language's conventions

	Symbols []Symbol
	Imports []Import
	Refs    []Ref
	Exports []Export
	Hints   []BindingHint
}

// Symbol is one declaration.
type Symbol struct {
	Name string
	Kind string
	// Receiver is the owning type expression of a method, field or
	// constructor: the Go receiver ("*Engine"), or the enclosing class.
	Receiver  string
	Signature string
	Doc       string
	Line      int
	EndLine   int
	Exported  bool // visible outside its unit (Go: capitalised; else VisPublic)
	// Container is 1 + the File.Symbols index of the enclosing symbol (an
	// interface for its method specs, a struct or interface for its embeds,
	// a class for its members); 0 means top level. The offset keeps the
	// zero value meaning "none".
	Container  int
	Visibility uint8  // Vis* constant
	Modifiers  uint32 // Mod* bits
}

// Parent returns the File.Symbols index of the enclosing symbol, if any.
func (s Symbol) Parent() (int, bool) { return s.Container - 1, s.Container > 0 }

// Qualified returns Recv.Name for members (receiver without '*' or type
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
		if recv[i] == '[' || recv[i] == '<' {
			return recv[:i]
		}
	}
	return recv
}

// Import is one import declaration.
type Import struct {
	Path  string // the spec: module path, package, header or file
	Name  string // local alias of the whole import ("" when absent; Go "_" or ".")
	Line  int
	Kind  uint8          // Import* constant
	Names []ImportedName // names imported from the spec, if listed
}

// ImportedName is one listed name of an import: from x import a as b.
type ImportedName struct {
	Name  string
	Alias string // "" when not renamed
}

// Export makes a name visible under the file's unit: JS/TS export … from,
// Python __all__, Rust pub use. Declarations that are simply public are not
// exports; their Visibility says so.
type Export struct {
	Name       string // exported name; "*" re-exports everything from Source
	Source     string // import spec it comes from; "" for a local name
	SourceName string // name in Source, when renamed (export { a as b } from)
	Line       int
}

// Ref is one unresolved reference. Resolution happens at query time.
type Ref struct {
	Kind      uint8 // Ref* constant
	Enclosing int   // index into File.Symbols, or NoCaller
	Name      string
	Qualifier string
	QualKind  uint8
	Line      int
	Col       int
}

// BindingHint records the declared or constructed type of a local variable
// or field, so x.Save() can resolve to Repo.Save when x is a Repo:
// x := &Engine{}, Foo x = new Foo(), self.repo: Repo.
type BindingHint struct {
	Scope int    // index into File.Symbols of the enclosing symbol, or NoCaller
	Name  string // local or field name ("x", "self.repo")
	Type  string // type name as written, without pointer or generic syntax
	Line  int
}
