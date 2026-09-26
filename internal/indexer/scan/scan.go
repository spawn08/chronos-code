// Package scan discovers indexable files under a workspace root and applies
// the repository's ignore rules.
package scan

import (
	"bufio"
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/indexer/extract/manifest"
	"github.com/spawn08/chronos-code/internal/indexer/extract/packs"
)

const gitTimeout = 10 * time.Second

// SkipDir reports directory names that are never indexed.
func SkipDir(name string) bool {
	if name == "" {
		return false
	}
	return (name[0] == '.' && name != ".") || name == "vendor" || name == "node_modules" || name == "testdata"
}

// Indexable reports whether a root-relative, slash-separated path is a
// file the indexer handles: Go, a language pack's extension, or a project
// manifest (package.json, Cargo.toml, ...).
func Indexable(rel string) bool {
	if !strings.HasSuffix(rel, ".go") && packs.Default().ForPath(rel) == nil && manifest.Kind(rel) == "" {
		return false
	}
	for _, dir := range strings.Split(path.Dir(rel), "/") {
		if SkipDir(dir) {
			return false
		}
	}
	return true
}

// IsModuleFile reports go.mod files outside skipped directories.
func IsModuleFile(rel string) bool {
	if path.Base(rel) != "go.mod" {
		return false
	}
	for _, dir := range strings.Split(path.Dir(rel), "/") {
		if SkipDir(dir) {
			return false
		}
	}
	return true
}

// Listing is the result of a full discovery pass.
type Listing struct {
	Sources []string // indexable files, root-relative
	Modules []string // go.mod files, root-relative
	Git     bool     // ignore rules came from git
}

// List enumerates tracked and untracked, non-ignored files with git, falling
// back to a directory walk outside a repository.
func List(ctx context.Context, root string) (Listing, error) {
	if out, err := git(ctx, root, nil, "ls-files", "-z", "--cached", "--others", "--exclude-standard"); err == nil {
		l := Listing{Git: true}
		for _, rel := range bytes.Split(out, []byte{0}) {
			l.add(string(rel))
		}
		return l, nil
	}
	var l Listing
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != root && SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err == nil {
			l.add(filepath.ToSlash(rel))
		}
		return nil
	})
	return l, err
}

func (l *Listing) add(rel string) {
	switch {
	case rel == "":
	case Indexable(rel):
		l.Sources = append(l.Sources, rel)
	case IsModuleFile(rel):
		l.Modules = append(l.Modules, rel)
	}
}

// Ignored returns the subset of rels that git ignores. Outside a repository
// nothing is ignored.
func Ignored(ctx context.Context, root string, rels []string) map[string]bool {
	out := map[string]bool{}
	if len(rels) == 0 {
		return out
	}
	var in bytes.Buffer
	for _, r := range rels {
		in.WriteString(r)
		in.WriteByte(0)
	}
	res, err := git(ctx, root, &in, "check-ignore", "-z", "--stdin")
	if err != nil && len(res) == 0 {
		return out // exit status 1 means "none ignored"; other failures: treat as not ignored
	}
	for _, r := range bytes.Split(res, []byte{0}) {
		if len(r) > 0 {
			out[string(r)] = true
		}
	}
	return out
}

func git(ctx context.Context, root string, stdin *bytes.Buffer, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	return cmd.Output()
}

// ModulePath reads the module path declared in a go.mod file.
func ModulePath(gomod string) string {
	f, err := os.Open(gomod)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}
