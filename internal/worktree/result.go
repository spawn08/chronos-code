package worktree

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

type Cleanup struct {
	State        string `json:"state"`
	ManifestPath string `json:"manifest_path,omitempty"`
}

// Result is the transport-safe output of an isolated attempt. Patch is raw
// binary git patch data and must never be converted through a string protocol.
type Result struct {
	TaskID       string   `json:"task_id"`
	AttemptID    string   `json:"attempt_id"`
	BaseRevision string   `json:"base_revision"`
	FinalTree    string   `json:"final_tree"`
	FinalHash    string   `json:"final_hash"`
	ChangedPaths []string `json:"changed_paths"`
	Patch        []byte   `json:"patch"`
	Checks       []Check  `json:"checks,omitempty"`
	Cleanup      Cleanup  `json:"cleanup"`
}

// Collect stages the private worktree only, allowing tracked, untracked, and
// deleted files to be represented by one binary-safe patch and tree object.
func (m *Manager) Collect(ctx context.Context, handle Handle, checks []Check) (Result, error) {
	manifest := handle.Manifest
	if err := m.validateOwned(manifest); err != nil {
		return Result{}, err
	}
	if _, err := m.git(ctx, manifest.WorktreePath, "add", "-A"); err != nil {
		return Result{}, fmt.Errorf("stage isolated result: %w", err)
	}
	tree, err := m.git(ctx, manifest.WorktreePath, "write-tree")
	if err != nil {
		return Result{}, fmt.Errorf("write isolated result tree: %w", err)
	}
	patch, err := m.git(ctx, manifest.WorktreePath, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", manifest.BaseRevision, "--")
	if err != nil {
		return Result{}, fmt.Errorf("collect isolated patch: %w", err)
	}
	names, err := m.git(ctx, manifest.WorktreePath, "diff", "--cached", "--name-only", "-z", manifest.BaseRevision, "--")
	if err != nil {
		return Result{}, fmt.Errorf("collect changed paths: %w", err)
	}
	changed := splitNUL(names.Stdout)
	sort.Strings(changed)
	hash := sha256.Sum256(patch.Stdout)
	return Result{
		TaskID: manifest.TaskID, AttemptID: manifest.AttemptID, BaseRevision: manifest.BaseRevision,
		FinalTree: strings.TrimSpace(string(tree.Stdout)), FinalHash: fmt.Sprintf("sha256:%x", hash[:]),
		ChangedPaths: changed, Patch: append([]byte(nil), patch.Stdout...), Checks: append([]Check(nil), checks...),
		Cleanup: Cleanup{State: string(manifest.CleanupState), ManifestPath: manifest.ManifestPath},
	}, nil
}

func splitNUL(data []byte) []string {
	parts := strings.Split(string(data), "\x00")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}
