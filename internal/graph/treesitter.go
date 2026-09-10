//go:build treesitter

// This file implements PRD P2-012: Tier-2 (syntactic, non-type-checked) code
// graph indexing for TypeScript, Python, Rust, and Java via tree-sitter,
// behind the "treesitter" build tag so the default build (Tier-1 Go only)
// never pulls in tree-sitter's cgo dependency. Build with:
//
//	go build -tags treesitter ./...
package graph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/bash"
	"github.com/smacker/go-tree-sitter/c"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/csharp"
	"github.com/smacker/go-tree-sitter/css"
	"github.com/smacker/go-tree-sitter/html"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/smacker/go-tree-sitter/php"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/ruby"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/sql"
	"github.com/smacker/go-tree-sitter/swift"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
	"github.com/smacker/go-tree-sitter/yaml"
)

// SupportedTreeSitterExtensions reports the file extensions handled by the
// tree-sitter Tier-2 parsers.
func SupportedTreeSitterExtensions() []string {
	return []string{
		".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".java",
		".c", ".h",
		".cpp", ".hpp", ".cc", ".cxx",
		".cs",
		".rb",
		".php",
		".kt", ".kts",
		".swift",
		".sh", ".bash",
		".sql",
		".yaml", ".yml",
		".html", ".htm",
		".css",
	}
}

// tsLangSpec maps a tree-sitter grammar's node type names onto our own
// Symbol/Edge vocabulary. Node type names are grammar-specific and were
// confirmed empirically against each grammar (not guessed) before writing
// this table.
type tsLangSpec struct {
	lang        *sitter.Language
	importTypes map[string]bool
	declKinds   map[string]SymbolKind
	// nameFor extracts the declaration name from a matched node when the
	// grammar doesn't expose it via ChildByFieldName("name"). If nil, the
	// default "name" field lookup is used.
	nameFor func(n *sitter.Node, src []byte) string
	// kindFor refines the SymbolKind for a matched node (e.g. Kotlin uses
	// class_declaration for both classes and interfaces). If nil, the
	// declKinds map value is used as-is.
	kindFor func(n *sitter.Node, src []byte, fallback SymbolKind) SymbolKind
	// funcContainerTypes marks node types that introduce a new enclosing
	// function/method name for call-edge attribution (e.g.
	// function_declaration). Nil means call edges are not attempted for
	// this language.
	funcContainerTypes map[string]bool
	// callType is the grammar's call-expression node type (e.g.
	// "call_expression" for JS/TS, "call" for Python). Empty means call
	// edges are not attempted for this language.
	callType string
	// calleeFor extracts the callee's short name from a matched callType
	// node. Resolution is name-based, matching the same "acceptable
	// precision for a zero-LLM-cost navigation aid" tradeoff callEdges
	// documents for the Go Tier-1 indexer.
	calleeFor func(n *sitter.Node, src []byte) string
	// resolveImportTargets extracts a matched importTypes node's module
	// specifier(s) and resolves any that point at a file inside root to a
	// package identifier (relative directory, or "(root)"). Specifiers that
	// can't be resolved to an in-repo file (external packages, unresolvable
	// relative imports) are silently dropped rather than guessed. Nil means
	// import edges are not attempted for this language.
	resolveImportTargets func(n *sitter.Node, src []byte, root, fileDir string) []string
}

func tsLangFor(ext string) *tsLangSpec {
	switch ext {
	case ".ts", ".tsx":
		return &tsLangSpec{
			lang:        typescript.GetLanguage(),
			importTypes: map[string]bool{"import_statement": true},
			declKinds: map[string]SymbolKind{
				"function_declaration":  KindFunc,
				"method_definition":     KindMethod,
				"class_declaration":     KindStruct,
				"interface_declaration": KindInterface,
			},
			funcContainerTypes:   map[string]bool{"function_declaration": true, "method_definition": true},
			callType:             "call_expression",
			calleeFor:            jsCalleeName,
			resolveImportTargets: resolveJSImportTargets,
		}
	case ".js", ".jsx":
		return &tsLangSpec{
			lang:        javascript.GetLanguage(),
			importTypes: map[string]bool{"import_statement": true},
			declKinds: map[string]SymbolKind{
				"function_declaration": KindFunc,
				"method_definition":    KindMethod,
				"class_declaration":    KindStruct,
			},
			funcContainerTypes:   map[string]bool{"function_declaration": true, "method_definition": true},
			callType:             "call_expression",
			calleeFor:            jsCalleeName,
			resolveImportTargets: resolveJSImportTargets,
		}
	case ".py":
		return &tsLangSpec{
			lang: python.GetLanguage(),
			importTypes: map[string]bool{
				"import_statement":      true,
				"import_from_statement": true,
			},
			declKinds: map[string]SymbolKind{
				"function_definition": KindFunc,
				"class_definition":    KindStruct,
			},
			funcContainerTypes:   map[string]bool{"function_definition": true},
			callType:             "call",
			calleeFor:            pyCalleeName,
			resolveImportTargets: resolvePyImportTargets,
		}
	case ".rs":
		return &tsLangSpec{
			lang:        rust.GetLanguage(),
			importTypes: map[string]bool{"use_declaration": true},
			declKinds: map[string]SymbolKind{
				"function_item":           KindFunc,
				"function_signature_item": KindFunc,
				"struct_item":             KindStruct,
				"trait_item":              KindInterface,
			},
		}
	case ".java":
		return &tsLangSpec{
			lang:        java.GetLanguage(),
			importTypes: map[string]bool{"import_declaration": true},
			declKinds: map[string]SymbolKind{
				"method_declaration":    KindMethod,
				"class_declaration":     KindStruct,
				"interface_declaration": KindInterface,
			},
		}
	case ".c", ".h":
		return &tsLangSpec{
			lang:        c.GetLanguage(),
			importTypes: map[string]bool{"preproc_include": true},
			declKinds: map[string]SymbolKind{
				"function_definition": KindFunc,
				"struct_specifier":    KindStruct,
			},
			nameFor: cNameFor,
		}
	case ".cpp", ".hpp", ".cc", ".cxx":
		return &tsLangSpec{
			lang: cpp.GetLanguage(),
			importTypes: map[string]bool{
				"preproc_include":   true,
				"using_declaration": true,
			},
			declKinds: map[string]SymbolKind{
				"function_definition": KindFunc,
				"class_specifier":     KindStruct,
			},
			nameFor: cppNameFor,
		}
	case ".cs":
		return &tsLangSpec{
			lang:        csharp.GetLanguage(),
			importTypes: map[string]bool{"using_directive": true},
			declKinds: map[string]SymbolKind{
				"method_declaration":    KindMethod,
				"class_declaration":     KindStruct,
				"interface_declaration": KindInterface,
			},
		}
	case ".rb":
		return &tsLangSpec{
			lang: ruby.GetLanguage(),
			declKinds: map[string]SymbolKind{
				"method": KindMethod,
				"class":  KindStruct,
				"module": KindType,
			},
		}
	case ".php":
		return &tsLangSpec{
			lang:        php.GetLanguage(),
			importTypes: map[string]bool{"namespace_use_declaration": true},
			declKinds: map[string]SymbolKind{
				"function_definition": KindFunc,
				"method_declaration":  KindMethod,
				"class_declaration":   KindStruct,
			},
		}
	case ".kt", ".kts":
		return &tsLangSpec{
			lang:        kotlin.GetLanguage(),
			importTypes: map[string]bool{"import_header": true},
			declKinds: map[string]SymbolKind{
				"function_declaration":  KindFunc,
				"class_declaration":     KindStruct,
				"interface_declaration": KindInterface,
			},
			nameFor: kotlinNameFor,
			kindFor: kotlinKindFor,
		}
	case ".swift":
		return &tsLangSpec{
			lang:        swift.GetLanguage(),
			importTypes: map[string]bool{"import_declaration": true},
			declKinds: map[string]SymbolKind{
				"function_declaration": KindFunc,
				"class_declaration":    KindStruct,
				"protocol_declaration": KindInterface,
			},
		}
	case ".sh", ".bash":
		return &tsLangSpec{
			lang: bash.GetLanguage(),
			declKinds: map[string]SymbolKind{
				"function_definition": KindFunc,
			},
		}
	case ".sql":
		return &tsLangSpec{
			lang: sql.GetLanguage(),
		}
	case ".yaml", ".yml":
		return &tsLangSpec{
			lang: yaml.GetLanguage(),
		}
	case ".html", ".htm":
		return &tsLangSpec{
			lang: html.GetLanguage(),
		}
	case ".css":
		return &tsLangSpec{
			lang: css.GetLanguage(),
		}
	default:
		return nil
	}
}

// cNameFor extracts declaration names from C grammar nodes. C's
// function_definition nests the name inside declarator → identifier;
// struct_specifier uses the "name" field directly (type_identifier).
func cNameFor(n *sitter.Node, src []byte) string {
	switch n.Type() {
	case "function_definition":
		if decl := n.ChildByFieldName("declarator"); decl != nil {
			return extractDeclaratorName(decl, src)
		}
	case "struct_specifier":
		if nameNode := n.ChildByFieldName("name"); nameNode != nil {
			return nameNode.Content(src)
		}
	}
	return ""
}

// cppNameFor extracts declaration names from C++ grammar nodes. Same
// declarator nesting as C for functions; class_specifier uses "name".
func cppNameFor(n *sitter.Node, src []byte) string {
	switch n.Type() {
	case "function_definition":
		if decl := n.ChildByFieldName("declarator"); decl != nil {
			return extractDeclaratorName(decl, src)
		}
	case "class_specifier":
		if nameNode := n.ChildByFieldName("name"); nameNode != nil {
			return nameNode.Content(src)
		}
	}
	return ""
}

// extractDeclaratorName walks a C/C++ declarator subtree to find the
// identifier. Handles function_declarator (wraps an identifier) and
// pointer_declarator (adds a * prefix).
func extractDeclaratorName(n *sitter.Node, src []byte) string {
	switch n.Type() {
	case "identifier", "field_identifier":
		return n.Content(src)
	case "function_declarator", "pointer_declarator", "parenthesized_declarator",
		"reference_declarator":
		if child := n.ChildByFieldName("declarator"); child != nil {
			return extractDeclaratorName(child, src)
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			if name := extractDeclaratorName(n.Child(i), src); name != "" {
				return name
			}
		}
	}
	return ""
}

// kotlinNameFor extracts declaration names from Kotlin grammar nodes.
// Kotlin uses type_identifier for class/interface names and
// simple_identifier for function names — not a "name" field.
func kotlinNameFor(n *sitter.Node, src []byte) string {
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		switch child.Type() {
		case "type_identifier", "simple_identifier":
			return child.Content(src)
		}
	}
	return ""
}

// kotlinKindFor refines the SymbolKind for Kotlin's class_declaration node:
// the grammar uses class_declaration for both classes and interfaces,
// distinguished by the keyword child ("interface" vs. "class").
func kotlinKindFor(n *sitter.Node, src []byte, fallback SymbolKind) SymbolKind {
	if n.Type() != "class_declaration" {
		return fallback
	}
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child.Type() == "interface" || child.Content(src) == "interface" {
			return KindInterface
		}
	}
	return fallback
}

// IndexNonGoFile parses relPath (resolved against root if not already
// absolute) with the matching tree-sitter grammar and records its top-level
// function/class/interface/struct declarations and import statements into
// store. This is a best-effort syntactic pass (no type-checking, unlike the
// Go Tier-1 indexer): declarations without a resolvable "name" field are
// skipped rather than guessed. For languages with callType/calleeFor set
// (currently JS/TS/Python), it also records name-based "call" edges and,
// for resolveImportTargets, "import" edges to other in-repo packages;
// implements-edge resolution remains out of scope for tree-sitter languages
// since it needs type information this syntactic pass doesn't have. Returns
// the number of symbols and edges recorded, and an error only for file-read
// or parse failures. An unsupported extension is not an error: it returns
// (0, 0, nil).
func IndexNonGoFile(ctx context.Context, store *Store, root, relPath string) (symbols, edges int, err error) {
	spec := tsLangFor(strings.ToLower(filepath.Ext(relPath)))
	if spec == nil {
		return 0, 0, nil
	}

	absPath := relPath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(root, relPath)
	}
	source, err := os.ReadFile(absPath)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", relPath, err)
	}

	parser := sitter.NewParser()
	parser.SetLanguage(spec.lang)
	tree, err := parser.ParseCtx(ctx, nil, source)
	if err != nil {
		return 0, 0, fmt.Errorf("parse %s: %w", relPath, err)
	}
	defer tree.Close()

	if err := store.ClearFile(ctx, relPath); err != nil {
		return 0, 0, fmt.Errorf("clear %s: %w", relPath, err)
	}

	pkg := filepath.Dir(relPath)
	if pkg == "." {
		pkg = "(root)"
	}

	var imports []string
	count := 0
	edgeCount := 0
	fileDir := filepath.Dir(relPath)
	// edgeSeen dedupes within this file's pass; InsertEdge is also
	// INSERT-OR-IGNORE-safe against the DB's unique index, but this avoids
	// redundant calls for e.g. a function that calls the same callee twice.
	edgeSeen := make(map[string]bool)

	// walk carries enclosing, the bare name of the nearest containing
	// function/method (per funcContainerTypes), so a call found inside it
	// can be attributed. Both call and import edges are name-based (package
	// identifiers for imports, bare symbol names for calls) rather than
	// fully resolved, matching the same precision tradeoff callEdges
	// documents for the Go Tier-1 indexer.
	var walk func(n *sitter.Node, enclosing string)
	walk = func(n *sitter.Node, enclosing string) {
		if n == nil {
			return
		}
		typ := n.Type()
		if spec.importTypes[typ] {
			if line := firstLine(n.Content(source)); line != "" {
				imports = append(imports, line)
			}
			if spec.resolveImportTargets != nil {
				for _, target := range spec.resolveImportTargets(n, source, root, fileDir) {
					if target == "" || target == pkg {
						continue
					}
					key := "import|" + target
					if edgeSeen[key] {
						continue
					}
					edgeSeen[key] = true
					if insertErr := store.InsertEdge(ctx, Edge{Kind: EdgeImport, FromName: pkg, ToName: target}); insertErr == nil {
						edgeCount++
					}
				}
			}
		}
		if typ == spec.callType && spec.calleeFor != nil && enclosing != "" {
			if callee := spec.calleeFor(n, source); callee != "" && callee != enclosing {
				key := "call|" + enclosing + "|" + callee
				if !edgeSeen[key] {
					edgeSeen[key] = true
					if insertErr := store.InsertEdge(ctx, Edge{Kind: EdgeCall, FromName: enclosing, ToName: callee}); insertErr == nil {
						edgeCount++
					}
				}
			}
		}
		nextEnclosing := enclosing
		if kind, ok := spec.declKinds[typ]; ok {
			if spec.kindFor != nil {
				kind = spec.kindFor(n, source, kind)
			}
			var name string
			if spec.nameFor != nil {
				name = spec.nameFor(n, source)
			} else if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name = nameNode.Content(source)
			}
			if name != "" {
				sym := Symbol{
					Name:      name,
					Kind:      kind,
					Package:   pkg,
					File:      relPath,
					Line:      int(n.StartPoint().Row) + 1,
					EndLine:   int(n.EndPoint().Row) + 1,
					Signature: firstLine(n.Content(source)),
				}
				if insertErr := store.InsertSymbol(ctx, sym); insertErr == nil {
					count++
				}
				if spec.funcContainerTypes[typ] {
					nextEnclosing = name
				}
			}
		}
		childCount := int(n.ChildCount())
		for i := 0; i < childCount; i++ {
			walk(n.Child(i), nextEnclosing)
		}
	}
	walk(tree.RootNode(), "")

	mtime := time.Now().Unix()
	if info, statErr := os.Stat(absPath); statErr == nil {
		mtime = info.ModTime().Unix()
	}
	if err := store.UpsertFile(ctx, relPath, pkg, mtime); err != nil {
		return count, edgeCount, err
	}
	if err := store.UpsertPackage(ctx, pkg, strings.Join(imports, ",")); err != nil {
		return count, edgeCount, err
	}

	return count, edgeCount, nil
}

// firstLine returns the first line of s, trimmed, truncating very long
// single-line declarations to a reasonable signature length.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// jsCalleeName extracts the callee's short name from a JS/TS call_expression
// node: the bare name for a direct call (foo()) or the rightmost property
// for a member call (a.b.foo()).
func jsCalleeName(n *sitter.Node, src []byte) string {
	fn := n.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Type() {
	case "identifier":
		return fn.Content(src)
	case "member_expression":
		if prop := fn.ChildByFieldName("property"); prop != nil {
			return prop.Content(src)
		}
	}
	return ""
}

// pyCalleeName extracts the callee's short name from a Python call node: the
// bare name for a direct call (foo()) or the attribute for a method call
// (a.b.foo()).
func pyCalleeName(n *sitter.Node, src []byte) string {
	fn := n.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Type() {
	case "identifier":
		return fn.Content(src)
	case "attribute":
		if attr := fn.ChildByFieldName("attribute"); attr != nil {
			return attr.Content(src)
		}
	}
	return ""
}

// unquoteJSString strips the surrounding quote characters (', ", `) from a
// JS/TS string node's raw content.
func unquoteJSString(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if last == first && (first == '"' || first == '\'' || first == '`') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// jsImportSpecifier extracts the raw module specifier string from a JS/TS
// import_statement node — the string literal naming the module, whether the
// statement has a "from" clause or is a bare side-effect import.
func jsImportSpecifier(n *sitter.Node, src []byte) string {
	for i := 0; i < int(n.ChildCount()); i++ {
		if c := n.Child(i); c.Type() == "string" {
			return unquoteJSString(c.Content(src))
		}
	}
	return ""
}

// resolveJSImportTargets resolves a JS/TS import_statement's specifier to an
// in-repo package identifier. Bare specifiers (npm packages, path aliases)
// aren't attempted — only relative ("./x", "../x") specifiers that resolve
// to an actual file under root.
func resolveJSImportTargets(n *sitter.Node, src []byte, root, fileDir string) []string {
	spec := jsImportSpecifier(n, src)
	if spec == "" || !strings.HasPrefix(spec, ".") {
		return nil
	}
	if dir, ok := resolveJSImportTarget(root, fileDir, spec); ok {
		return []string{dir}
	}
	return nil
}

func resolveJSImportTarget(root, fileDir, specifier string) (string, bool) {
	candidate := filepath.Clean(filepath.Join(fileDir, specifier))
	if candidate == ".." || strings.HasPrefix(candidate, "../") {
		return "", false
	}
	exts := []string{"", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}
	for _, ext := range exts {
		if info, err := os.Stat(filepath.Join(root, candidate+ext)); err == nil && !info.IsDir() {
			return normalizePkgDir(filepath.Dir(candidate)), true
		}
	}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"} {
		if info, err := os.Stat(filepath.Join(root, candidate, "index"+ext)); err == nil && !info.IsDir() {
			return normalizePkgDir(candidate), true
		}
	}
	return "", false
}

// pyModuleNameParts extracts the dotted module path and relative-import
// level (number of leading dots; 0 for an absolute import) from an
// import_from_statement's module_name field, which is either a plain
// dotted_name or a relative_import (dots plus an optional dotted_name, e.g.
// "from ..pkg import x").
func pyModuleNameParts(mod *sitter.Node, src []byte) (dotted string, level int) {
	if mod.Type() == "dotted_name" {
		return mod.Content(src), 0
	}
	for i := 0; i < int(mod.ChildCount()); i++ {
		switch c := mod.Child(i); c.Type() {
		case "import_prefix":
			level = len(c.Content(src))
		case "dotted_name":
			dotted = c.Content(src)
		}
	}
	return dotted, level
}

// resolvePyImportTargets resolves a Python import_statement or
// import_from_statement's module name(s) to in-repo package identifiers.
// Absolute imports are tried relative to root (the common src-at-root
// layout); anything unresolvable there is dropped rather than guessed,
// since Python's actual sys.path search order isn't visible to a syntactic
// pass.
func resolvePyImportTargets(n *sitter.Node, src []byte, root, fileDir string) []string {
	var out []string
	switch n.Type() {
	case "import_from_statement":
		mod := n.ChildByFieldName("module_name")
		if mod == nil {
			return nil
		}
		dotted, level := pyModuleNameParts(mod, src)
		if dir, ok := resolvePyImportTarget(root, fileDir, dotted, level); ok {
			out = append(out, dir)
		}
	case "import_statement":
		for i := 0; i < int(n.ChildCount()); i++ {
			c := n.Child(i)
			var dotted string
			switch c.Type() {
			case "dotted_name":
				dotted = c.Content(src)
			case "aliased_import":
				if name := c.ChildByFieldName("name"); name != nil {
					dotted = name.Content(src)
				}
			default:
				continue
			}
			if dir, ok := resolvePyImportTarget(root, fileDir, dotted, 0); ok {
				out = append(out, dir)
			}
		}
	}
	return out
}

func resolvePyImportTarget(root, fileDir, dotted string, level int) (string, bool) {
	if level == 0 && dotted == "" {
		return "", false
	}
	baseDir := "."
	if level > 0 {
		baseDir = fileDir
		for i := 0; i < level-1; i++ {
			baseDir = filepath.Dir(baseDir)
		}
	}
	var candidate string
	if dotted == "" {
		candidate = filepath.Clean(baseDir)
	} else {
		candidate = filepath.Clean(filepath.Join(baseDir, filepath.Join(strings.Split(dotted, ".")...)))
	}
	if candidate == ".." || strings.HasPrefix(candidate, "../") {
		return "", false
	}
	abs := filepath.Join(root, candidate)
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		return normalizePkgDir(candidate), true
	}
	if info, err := os.Stat(abs + ".py"); err == nil && !info.IsDir() {
		return normalizePkgDir(filepath.Dir(candidate)), true
	}
	return "", false
}

// normalizePkgDir matches the "(root)" convention IndexNonGoFile uses for a
// file directly under root, so resolved import targets compare equal to the
// pkg identifier recorded for that file's package.
func normalizePkgDir(dir string) string {
	if dir == "." {
		return "(root)"
	}
	return dir
}
