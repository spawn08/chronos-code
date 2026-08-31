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
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// SupportedTreeSitterExtensions reports the file extensions handled by the
// tree-sitter Tier-2 parsers.
func SupportedTreeSitterExtensions() []string {
	return []string{".ts", ".tsx", ".py", ".rs", ".java"}
}

// tsLangSpec maps a tree-sitter grammar's node type names onto our own
// Symbol/Edge vocabulary. Node type names are grammar-specific and were
// confirmed empirically against each grammar (not guessed) before writing
// this table.
type tsLangSpec struct {
	lang        *sitter.Language
	importTypes map[string]bool
	declKinds   map[string]SymbolKind
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
	default:
		return nil
	}
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
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				sym := Symbol{
					Name:      nameNode.Content(source),
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
