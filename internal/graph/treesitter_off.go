//go:build !treesitter

package graph

import "context"

// SupportedTreeSitterExtensions reports the file extensions the Tier-2
// tree-sitter parsers handle. The default build (no "treesitter" build tag)
// excludes the tree-sitter/cgo dependency entirely to keep the binary small
// (PRD "binary size" goals), so this reports no supported extensions and
// IndexNonGoFile below is a no-op. Build with `-tags treesitter` to enable
// multi-language indexing (PRD P2-012).
func SupportedTreeSitterExtensions() []string { return nil }

// IndexNonGoFile is a no-op in the default (non-treesitter) build.
func IndexNonGoFile(ctx context.Context, store *Store, root, relPath string) (symbols, edges int, err error) {
	return 0, 0, nil
}
