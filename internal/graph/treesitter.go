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
		".ts", ".tsx", ".py", ".rs", ".java",
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
				"method":  KindMethod,
				"class":   KindStruct,
				"module":  KindType,
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
// skipped rather than guessed. Returns the number of symbols recorded (edges
// is always 0 for now — cross-file call/implements resolution for these
// languages is out of scope; only containment/import data is recorded) and
// an error only for file-read or parse failures. An unsupported extension is
// not an error: it returns (0, 0, nil).
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

	pkg := filepath.Dir(relPath)
	if pkg == "." {
		pkg = "(root)"
	}

	var imports []string
	count := 0

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		typ := n.Type()
		if spec.importTypes[typ] {
			if line := firstLine(n.Content(source)); line != "" {
				imports = append(imports, line)
			}
		}
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
			}
		}
		childCount := int(n.ChildCount())
		for i := 0; i < childCount; i++ {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())

	mtime := time.Now().Unix()
	if info, statErr := os.Stat(absPath); statErr == nil {
		mtime = info.ModTime().Unix()
	}
	if err := store.UpsertFile(ctx, relPath, pkg, mtime); err != nil {
		return count, 0, err
	}
	if err := store.UpsertPackage(ctx, pkg, strings.Join(imports, ",")); err != nil {
		return count, 0, err
	}

	return count, 0, nil
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
