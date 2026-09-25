package worktree

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxAuthorizedDirtyBytes = 32 << 20

type dirtyInput struct {
	path        string
	content     []byte
	fingerprint FileFingerprint
}

// captureAuthorizedDirty takes only paths both dirty and explicitly approved
// by the host. An unapproved path is never opened or copied into the agent's
// worktree, even though git status still reports its name to the controller.
func captureAuthorizedDirty(root string, dirty, allowed []string) ([]dirtyInput, error) {
	if len(allowed) == 0 {
		return nil, nil
	}
	if len(allowed) > 64 {
		return nil, fmt.Errorf("too many authorized dirty paths")
	}
	dirtySet := make(map[string]bool, len(dirty))
	for _, path := range dirty {
		dirtySet[path] = true
	}
	seen := make(map[string]bool, len(allowed))
	var inputs []dirtyInput
	remaining := int64(maxAuthorizedDirtyBytes)
	for _, path := range allowed {
		if err := validateRelativePath(path); err != nil {
			return nil, err
		}
		if seen[path] {
			return nil, fmt.Errorf("duplicate authorized dirty path %q", path)
		}
		seen[path] = true
		if !dirtySet[path] {
			continue
		}
		if err := rejectSymlinkTraversal(root, path); err != nil {
			return nil, err
		}
		fingerprint, err := fingerprintFile(root, path)
		if err != nil {
			return nil, err
		}
		input := dirtyInput{path: path, fingerprint: fingerprint}
		if fingerprint.Exists {
			fs, err := os.OpenRoot(root)
			if err != nil {
				return nil, err
			}
			file, err := fs.Open(path)
			if err != nil {
				fs.Close()
				return nil, err
			}
			input.content, err = io.ReadAll(io.LimitReader(file, remaining+1))
			file.Close()
			fs.Close()
			if err != nil {
				return nil, fmt.Errorf("read authorized dirty input %q: %w", path, err)
			}
			if int64(len(input.content)) > remaining {
				return nil, fmt.Errorf("authorized dirty input %q exceeds remaining size limit", path)
			}
			hash := sha256.Sum256(input.content)
			if fmt.Sprintf("%x", hash[:]) != fingerprint.Hash {
				return nil, fmt.Errorf("authorized dirty input %q changed during capture", path)
			}
			remaining -= int64(len(input.content))
		}
		inputs = append(inputs, input)
	}
	return inputs, nil
}

func (m *Manager) composeDirty(ctx context.Context, manifest *Manifest, inputs []dirtyInput) error {
	if len(manifest.InputArtifacts) > 0 {
		changes, err := m.git(ctx, manifest.WorktreePath, "diff", "--name-only", "-z", manifest.ParentRevision, manifest.BaseRevision, "--")
		if err != nil {
			return fmt.Errorf("inspect accepted predecessor paths: %w", err)
		}
		for _, input := range inputs {
			for _, predecessor := range splitNUL(changes.Stdout) {
				if pathsOverlap(input.path, predecessor) {
					return fmt.Errorf("authorized dirty input %q conflicts with accepted predecessor %q", input.path, predecessor)
				}
			}
		}
	}
	for _, input := range inputs {
		current, err := fingerprintFile(manifest.RepoRoot, input.path)
		if err != nil || current != input.fingerprint {
			return fmt.Errorf("authorized dirty input %q changed before composition", input.path)
		}
		if err := rejectSymlinkTraversal(manifest.WorktreePath, input.path); err != nil {
			return err
		}
		path := filepath.Join(manifest.WorktreePath, input.path)
		if input.fingerprint.Exists {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return fmt.Errorf("create private dirty input parent: %w", err)
			}
			if err := os.WriteFile(path, input.content, os.FileMode(input.fingerprint.Mode)); err != nil {
				return fmt.Errorf("write private dirty input: %w", err)
			}
			if err := os.Chmod(path, os.FileMode(input.fingerprint.Mode)); err != nil {
				return fmt.Errorf("set private dirty input mode: %w", err)
			}
		} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove privately deleted input: %w", err)
		}
	}
	args := []string{"add", "-A", "-f", "--"}
	for _, input := range inputs {
		args = append(args, input.path)
	}
	if _, err := m.git(ctx, manifest.WorktreePath, args...); err != nil {
		return fmt.Errorf("stage private dirty snapshot: %w", err)
	}
	if err := m.commitPrivateSnapshot(ctx, manifest, "chronos-code private authorized dirty input snapshot\n"); err != nil {
		return err
	}
	manifest.InputDirty = make(map[string]FileFingerprint, len(inputs))
	for _, input := range inputs {
		manifest.InputDirty[input.path] = input.fingerprint
	}
	return nil
}
