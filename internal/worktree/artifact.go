package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxAcceptedPatchBytes = 32 << 20

func (m *Manager) artifactDir() string { return filepath.Join(m.dataDir, "artifacts", "patches") }

func artifactPath(dir, id string) (string, error) {
	if !strings.HasPrefix(id, "sha256:") || len(id) != len("sha256:")+64 {
		return "", fmt.Errorf("invalid accepted artifact reference")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(id, "sha256:")); err != nil {
		return "", fmt.Errorf("decode accepted artifact identity: %w", err)
	}
	return filepath.Join(dir, strings.TrimPrefix(id, "sha256:")+".patch"), nil
}

// StoreArtifact retains a verified binary patch before applying it to the
// parent. The path is content-addressed, private, and independent of worktree
// cleanup or the user's index and branch.
func (m *Manager) StoreArtifact(ctx context.Context, result Result) error {
	if len(result.Patch) == 0 || len(result.Patch) > maxAcceptedPatchBytes {
		return fmt.Errorf("accepted patch size must be between 1 and %d bytes", maxAcceptedPatchBytes)
	}
	path, err := artifactPath(m.artifactDir(), result.FinalHash)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(result.Patch)
	if result.FinalHash != fmt.Sprintf("sha256:%x", hash[:]) {
		return fmt.Errorf("accepted patch checksum mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(m.artifactDir(), 0o700); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}
	if err := os.Chmod(m.artifactDir(), 0o700); err != nil {
		return fmt.Errorf("secure artifact directory: %w", err)
	}
	if existing, err := m.LoadArtifact(ctx, result.FinalHash); err == nil {
		if !bytes.Equal(existing, result.Patch) {
			return fmt.Errorf("existing artifact differs from its content identity")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(m.artifactDir(), ".artifact-*")
	if err != nil {
		return fmt.Errorf("create private artifact: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(result.Patch)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write accepted artifact: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("publish accepted artifact: %w", err)
	}
	return nil
}

func (m *Manager) LoadArtifact(ctx context.Context, id string) ([]byte, error) {
	path, err := artifactPath(m.artifactDir(), id)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxAcceptedPatchBytes {
		return nil, fmt.Errorf("accepted artifact is not a bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read accepted artifact: %w", err)
	}
	hash := sha256.Sum256(data)
	if id != fmt.Sprintf("sha256:%x", hash[:]) {
		return nil, fmt.Errorf("accepted artifact checksum mismatch")
	}
	return data, nil
}

func (m *Manager) composeAccepted(ctx context.Context, manifest *Manifest, references []string) error {
	for _, id := range references {
		patch, err := m.LoadArtifact(ctx, id)
		if err != nil {
			return fmt.Errorf("load predecessor %q: %w", id, err)
		}
		for _, action := range [][]string{{"apply", "--check", "--binary", "-"}, {"apply", "--binary", "-"}} {
			if _, err := m.runner.Run(ctx, Command{Dir: manifest.WorktreePath, Args: action, Stdin: patch}); err != nil {
				return fmt.Errorf("apply accepted predecessor %q: %w", id, err)
			}
		}
	}
	if _, err := m.git(ctx, manifest.WorktreePath, "add", "-A"); err != nil {
		return fmt.Errorf("stage private predecessor snapshot: %w", err)
	}
	if err := m.commitPrivateSnapshot(ctx, manifest, "chronos-code private accepted predecessor snapshot\n"); err != nil {
		return err
	}
	manifest.InputArtifacts = append([]string(nil), references...)
	return nil
}

func (m *Manager) commitPrivateSnapshot(ctx context.Context, manifest *Manifest, message string) error {
	tree, err := m.git(ctx, manifest.WorktreePath, "write-tree")
	if err != nil {
		return fmt.Errorf("write private input tree: %w", err)
	}
	commit, err := m.runner.Run(ctx, Command{Dir: manifest.WorktreePath, Args: []string{"commit-tree", strings.TrimSpace(string(tree.Stdout)), "-p", manifest.BaseRevision},
		Stdin: []byte(message),
		Env:   []string{"GIT_AUTHOR_NAME=Chronos Code", "GIT_AUTHOR_EMAIL=chronos-code@local.invalid", "GIT_COMMITTER_NAME=Chronos Code", "GIT_COMMITTER_EMAIL=chronos-code@local.invalid"}})
	if err != nil {
		return fmt.Errorf("capture private input snapshot: %w", err)
	}
	base := strings.TrimSpace(string(commit.Stdout))
	if _, err := m.git(ctx, manifest.WorktreePath, "reset", "--hard", base); err != nil {
		return fmt.Errorf("activate private input snapshot: %w", err)
	}
	if manifest.ParentRevision == "" {
		manifest.ParentRevision = manifest.BaseRevision
	}
	manifest.BaseRevision = base
	return nil
}
