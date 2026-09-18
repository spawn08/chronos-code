package worktree

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Integrate applies only selected paths after all conflict checks pass. A
// successful integration also removes the private worktree, ref, and manifest.
func (m *Manager) Integrate(ctx context.Context, handle Handle, selected []string) (Result, error) {
	manifest := handle.Manifest
	if err := m.validateOwned(manifest); err != nil {
		return Result{}, err
	}
	if len(selected) == 0 {
		return Result{}, fmt.Errorf("at least one changed path must be selected")
	}
	selected = append([]string(nil), selected...)
	sort.Strings(selected)
	for i, path := range selected {
		if err := validateRelativePath(path); err != nil {
			return Result{}, err
		}
		if i > 0 && path == selected[i-1] {
			return Result{}, fmt.Errorf("duplicate selected path %q", path)
		}
		if err := rejectSymlinkTraversal(manifest.RepoRoot, path); err != nil {
			return Result{}, err
		}
	}

	head, err := m.git(ctx, manifest.RepoRoot, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("validate parent revision: %w", err)
	}
	if current := strings.TrimSpace(string(head.Stdout)); current != manifest.BaseRevision {
		return Result{}, fmt.Errorf("stale base: parent HEAD is %s, attempt is based on %s", current, manifest.BaseRevision)
	}
	dirtyResult, err := m.git(ctx, manifest.RepoRoot, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return Result{}, fmt.Errorf("inspect parent changes: %w", err)
	}
	dirty, err := parseStatusPaths(dirtyResult.Stdout)
	if err != nil {
		return Result{}, fmt.Errorf("inspect parent changes: %w", err)
	}
	for _, candidate := range selected {
		for _, parentPath := range dirty {
			if pathsOverlap(candidate, parentPath) {
				return Result{}, fmt.Errorf("parent dirty path %q overlaps selected path %q", parentPath, candidate)
			}
		}
	}

	// Staging affects only the private worktree and ensures new files are included.
	if _, err := m.git(ctx, manifest.WorktreePath, "add", "-A"); err != nil {
		return Result{}, fmt.Errorf("stage isolated result: %w", err)
	}
	args := []string{"diff", "--cached", "--binary", "--full-index", "--no-ext-diff", manifest.BaseRevision, "--"}
	args = append(args, selected...)
	patch, err := m.git(ctx, manifest.WorktreePath, args...)
	if err != nil {
		return Result{}, fmt.Errorf("build selected patch: %w", err)
	}
	if len(patch.Stdout) == 0 {
		return Result{}, fmt.Errorf("selected paths contain no changes")
	}
	nameArgs := []string{"diff", "--cached", "--name-only", "-z", manifest.BaseRevision, "--"}
	nameArgs = append(nameArgs, selected...)
	names, err := m.git(ctx, manifest.WorktreePath, nameArgs...)
	if err != nil {
		return Result{}, fmt.Errorf("validate selected patch paths: %w", err)
	}
	selectedSet := make(map[string]struct{}, len(selected))
	for _, path := range selected {
		selectedSet[path] = struct{}{}
	}
	actualPaths := splitNUL(names.Stdout)
	for _, path := range actualPaths {
		if _, ok := selectedSet[path]; !ok {
			return Result{}, fmt.Errorf("selected patch also modifies unselected path %q", path)
		}
	}
	tree, err := m.git(ctx, manifest.WorktreePath, "write-tree")
	if err != nil {
		return Result{}, fmt.Errorf("write isolated result tree: %w", err)
	}
	patchHash := sha256.Sum256(patch.Stdout)
	result := Result{
		TaskID: manifest.TaskID, AttemptID: manifest.AttemptID, BaseRevision: manifest.BaseRevision,
		FinalTree: strings.TrimSpace(string(tree.Stdout)), FinalHash: fmt.Sprintf("sha256:%x", patchHash[:]),
		ChangedPaths: actualPaths, Patch: append([]byte(nil), patch.Stdout...),
		Cleanup: Cleanup{State: string(manifest.CleanupState), ManifestPath: manifest.ManifestPath},
	}
	_, err = m.runner.Run(ctx, Command{Dir: manifest.RepoRoot, Args: []string{"apply", "--check", "--binary", "--whitespace=nowarn", "-"}, Stdin: patch.Stdout})
	if err != nil {
		return Result{}, fmt.Errorf("selected patch conflicts with parent: %w", err)
	}
	if _, err := m.runner.Run(ctx, Command{Dir: manifest.RepoRoot, Args: []string{"apply", "--binary", "--whitespace=nowarn", "-"}, Stdin: patch.Stdout}); err != nil {
		return Result{}, fmt.Errorf("apply selected patch after successful validation: %w", err)
	}

	if err := m.cleanup(ctx, handle); err != nil {
		result.Cleanup.State = string(CleanupPending)
		return result, fmt.Errorf("changes applied but isolated cleanup is pending: %w", err)
	}
	result.Cleanup = Cleanup{State: "complete"}
	return result, nil
}

func validateRelativePath(path string) error {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || path == "." || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe selected path %q", path)
	}
	return nil
}

func rejectSymlinkTraversal(root, relative string) error {
	current := root
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect selected path %q: %w", relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("selected path %q traverses symlink %q", relative, current)
		}
	}
	return nil
}

func pathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}
