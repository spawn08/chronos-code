package indexer

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

const gitTimeout = 30 * time.Second

func gitOut(ctx context.Context, root string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-C", root}, args...)...).Output()
}

// gitHead returns the commit HEAD points to, or an error outside a
// repository or on an unborn branch.
func gitHead(ctx context.Context, root string) (string, error) {
	out, err := gitOut(ctx, root, "rev-parse", "--verify", "-q", "HEAD")
	head := strings.TrimSpace(string(out))
	if err != nil || head == "" {
		return "", errors.New("no git HEAD")
	}
	return head, nil
}

// gitPaths runs a git command printing NUL-separated paths.
func gitPaths(ctx context.Context, root string, args ...string) ([]string, error) {
	out, err := gitOut(ctx, root, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			paths = append(paths, string(p))
		}
	}
	return paths, nil
}

// gitDirty returns the root-relative paths whose working-tree content may
// differ from HEAD: staged, modified, deleted and untracked (not ignored).
// With core.fsmonitor configured git answers this without statting every
// tracked file.
func gitDirty(ctx context.Context, root string) ([]string, error) {
	staged, err := gitPaths(ctx, root, "diff", "--cached", "--name-only", "-z", "--no-renames", "--relative", "HEAD")
	if err != nil {
		return nil, err
	}
	worktree, err := gitPaths(ctx, root, "ls-files", "-z", "--modified", "--deleted", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	return append(staged, worktree...), nil
}
