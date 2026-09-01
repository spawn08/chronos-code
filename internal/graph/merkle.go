package graph

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cespare/xxhash/v2"
)

// FileHash reads path and returns its content hash (XXH64, hex-encoded).
// It detects real content changes independent of mtime, which editors and
// checkouts often update without changing bytes.
func FileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return fmt.Sprintf("%016x", xxhash.Sum64(data)), nil
}

// MerkleTree holds a content hash for every file and directory discovered
// by BuildTree. Directory hashes fold in their sorted children's hashes, so
// any change under a directory changes that directory's hash too.
type MerkleTree struct {
	Files map[string]string // path -> file content hash
	Dirs  map[string]string // path -> directory hash
}

// DirMerkleHash combines a directory's children (name -> hash) into a
// single hash, independent of map iteration order.
func DirMerkleHash(childHashes map[string]string) string {
	names := make([]string, 0, len(childHashes))
	for name := range childHashes {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte(0)
		b.WriteString(childHashes[name])
		b.WriteByte(0)
	}
	return fmt.Sprintf("%016x", xxhash.Sum64String(b.String()))
}

// BuildTree walks root and hashes every file whose lowercased extension is
// in extensions (nil or empty means every file), then folds file hashes
// into directory hashes bottom-up. Directories named .git, vendor, and
// node_modules, and any dotfile-prefixed directory other than root itself,
// are skipped — matching indexNonGoFiles' walk exclusions.
func BuildTree(root string, extensions map[string]bool) (*MerkleTree, error) {
	tree := &MerkleTree{Files: map[string]string{}, Dirs: map[string]string{}}
	childrenOf := map[string]map[string]string{}
	var dirs []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == ".git" || name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			dirs = append(dirs, path)
			return nil
		}
		if len(extensions) > 0 && !extensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		hash, hashErr := FileHash(path)
		if hashErr != nil {
			// Skip unreadable files rather than aborting the whole walk —
			// a permissions error or a broken symlink shouldn't stop
			// indexing the rest of the tree.
			return nil
		}
		tree.Files[path] = hash
		parent := filepath.Dir(path)
		if childrenOf[parent] == nil {
			childrenOf[parent] = map[string]string{}
		}
		childrenOf[parent][filepath.Base(path)] = hash
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("build merkle tree for %s: %w", root, err)
	}

	// Fold directory hashes deepest-first so each directory can include its
	// already-computed child directories' hashes.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		hash := DirMerkleHash(childrenOf[dir])
		tree.Dirs[dir] = hash
		if dir == root {
			continue
		}
		parent := filepath.Dir(dir)
		if childrenOf[parent] == nil {
			childrenOf[parent] = map[string]string{}
		}
		childrenOf[parent][filepath.Base(dir)] = hash
	}
	return tree, nil
}

// DiffTree returns the file paths present in newTree whose hash differs
// from oldTree, including paths absent from oldTree entirely (added
// files). oldTree may be nil (treated as empty).
func DiffTree(oldTree, newTree *MerkleTree) []string {
	var changed []string
	for path, hash := range newTree.Files {
		if oldTree == nil || oldTree.Files[path] != hash {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}
