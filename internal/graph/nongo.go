package graph

import (
	"context"
	"fmt"
	"path/filepath"
)

// RefreshNonGo brings the store's tree-sitter tier (non-Go files) up to date
// without loading or type-checking Go packages: Go facts come from the
// chronos indexer. Any Go rows left by an earlier IndexAll are removed so
// the two sources never overlap. In builds without the treesitter tag it is
// a no-op.
func (ix *Indexer) RefreshNonGo(ctx context.Context) error {
	if len(SupportedTreeSitterExtensions()) == 0 {
		return nil
	}
	if err := ix.beginIndex(ctx); err != nil {
		return err
	}
	defer func() { <-ix.indexing }()
	snap, err := ix.scan(ctx)
	if err != nil {
		return err
	}
	oldFiles, err := ix.indexedFiles(ctx)
	if err != nil {
		return err
	}
	oldHashes := make(map[string]string, len(oldFiles))
	for path, file := range oldFiles {
		abs := path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(ix.Root, path)
		}
		if filepath.Ext(path) == ".go" || snap.files[abs] == "" {
			if err := ix.Store.RemoveFile(ctx, path); err != nil {
				return err
			}
			continue
		}
		oldHashes[path] = file.hash
	}
	nonGo := make(map[string]string, len(snap.files))
	for path, hash := range snap.files {
		if filepath.Ext(path) != ".go" {
			nonGo[path] = hash
		}
	}
	if err := ix.indexNonGoFiles(ctx, &IndexStats{}, nonGo, oldHashes); err != nil {
		return err
	}
	current, err := ix.indexedFiles(ctx)
	if err != nil {
		return err
	}
	live := make(map[string]bool, len(current))
	for _, file := range current {
		live[file.pkg] = true
	}
	pkgs, err := ix.Store.Packages(ctx)
	if err != nil {
		return fmt.Errorf("list stored packages: %w", err)
	}
	for _, pkg := range pkgs {
		if !live[pkg] {
			if err := ix.Store.RemovePackage(ctx, pkg); err != nil {
				return fmt.Errorf("remove stale package %s: %w", pkg, err)
			}
		}
	}
	return ix.Store.PruneStaleEdges(ctx)
}
