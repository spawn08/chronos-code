package incctx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/fsignore"
)

const fileGlobTool = "file_glob"

// globMaxResults bounds one file_glob result; the newest files come first.
const globMaxResults = 500

// WrapGlob replaces the SDK's filepath.Glob handler, which does not support
// "**": 41% of observed file_glob calls returned no matches for patterns such
// as "**/main.rs". The wrapper walks the tree with .gitignore and ecosystem
// ignore rules, supports ** and {a,b}, and returns the newest files first.
func WrapGlob(a *agent.Agent, root string, denied ...DeniedFunc) {
	def, ok := a.Tools.Get(fileGlobTool)
	if !ok {
		return
	}
	def.ParallelSafe = true
	def.Description = "Find files by path pattern, newest first. ** matches any number of directories (\"**/*.py\", \"src/**/test_*.ts\"), {a,b} alternates (\"**/*.{ts,tsx}\"), * and ? match within one path segment. " +
		"Respects .gitignore and skips dependency, build and cache directories of every ecosystem. Use path to search below a subdirectory. At most 500 results; check truncated."
	props := toolProperties(def)
	props["path"] = map[string]any{
		"type":        "string",
		"description": "Directory to search below; the pattern is relative to it. Defaults to the workspace root.",
	}
	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		pattern, _ := args["pattern"].(string)
		if pattern == "" {
			return nil, fmt.Errorf("file_glob: 'pattern' argument is required")
		}
		base, _ := args["path"].(string)
		if base == "" {
			base = "."
		}
		dir, err := resolvePath(ctx, root, base)
		if err != nil {
			return nil, fmt.Errorf("file_glob: %w", err)
		}
		pattern = filepath.ToSlash(pattern)
		if filepath.IsAbs(pattern) {
			if rel, relErr := filepath.Rel(dir, filepath.FromSlash(pattern)); relErr == nil && !strings.HasPrefix(rel, "..") {
				pattern = filepath.ToSlash(rel)
			} else {
				return nil, fmt.Errorf("file_glob: pattern must be relative to path")
			}
		}
		matches, truncated, err := globWalk(ctx, fsignore.New(workspaceRoot(ctx, root)), firstDenied(denied), dir, pattern)
		if err != nil {
			return nil, fmt.Errorf("file_glob: %w", err)
		}
		return map[string]any{"pattern": pattern, "path": dir, "matches": matches, "truncated": truncated}, nil
	}
}

type globHit struct {
	path    string
	modTime int64
}

func globWalk(ctx context.Context, ignore *fsignore.Matcher, denied DeniedFunc, dir, pattern string) ([]string, bool, error) {
	var hits []globHit
	entries := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil // unreadable entries are skipped, as in grep
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if entries++; entries > grepMaxEntries*10 {
			return errStopScan
		}
		if p == dir {
			return nil
		}
		if d.IsDir() {
			if ignore.Ignored(p, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || ignore.Ignored(p, false) || denied != nil && denied(p) {
			return nil
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !fsignore.Match(pattern, rel) {
			return nil
		}
		var mod int64
		if info, infoErr := d.Info(); infoErr == nil {
			mod = info.ModTime().UnixNano()
		}
		hits = append(hits, globHit{path: p, modTime: mod})
		return nil
	})
	truncated := errors.Is(err, errStopScan)
	if err != nil && !truncated {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, false, nil
		}
		return nil, false, err
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].modTime != hits[j].modTime {
			return hits[i].modTime > hits[j].modTime
		}
		return hits[i].path < hits[j].path
	})
	if len(hits) > globMaxResults {
		hits, truncated = hits[:globMaxResults], true
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.path
	}
	return out, truncated, nil
}
