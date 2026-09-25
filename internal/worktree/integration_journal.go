package worktree

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// lockIntegration serializes the parent's check/apply/receipt sequence across
// manager instances sharing the project data directory, not just goroutines in
// one process. The OS releases this lock on process death.
func (m *Manager) lockIntegration(ctx context.Context) (func(), error) {
	path := filepath.Join(m.dataDir, "worktrees", "integration.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create integration lock directory: %w", err)
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open integration lock: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			lock.Close()
			return nil, err
		}
		if ok, err := tryLockFile(lock); err != nil {
			lock.Close()
			return nil, fmt.Errorf("acquire integration lock: %w", err)
		} else if ok {
			return func() { _ = unlockFile(lock); _ = lock.Close() }, nil
		}
		select {
		case <-ctx.Done():
			lock.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func fingerprintFile(root, path string) (FileFingerprint, error) {
	directory, err := os.OpenRoot(root)
	if err != nil {
		return FileFingerprint{}, fmt.Errorf("open integration root: %w", err)
	}
	defer directory.Close()
	info, err := directory.Lstat(path)
	if os.IsNotExist(err) {
		return FileFingerprint{}, nil
	}
	if err != nil {
		return FileFingerprint{}, fmt.Errorf("inspect integration path %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return FileFingerprint{}, fmt.Errorf("integration path %q is not a regular file", path)
	}
	f, err := directory.Open(path)
	if err != nil {
		return FileFingerprint{}, fmt.Errorf("open integration path %q: %w", path, err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return FileFingerprint{}, fmt.Errorf("integration path %q changed during inspection", path)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return FileFingerprint{}, fmt.Errorf("hash integration path %q: %w", path, err)
	}
	return FileFingerprint{Exists: true, Hash: fmt.Sprintf("%x", hash.Sum(nil)), Mode: uint32(opened.Mode().Perm())}, nil
}

func (m *Manager) prepareIntegration(manifest *Manifest, selected, paths []string, artifactID string, checks []Check) error {
	journal := &IntegrationJournal{State: "prepared", ArtifactID: artifactID, Selected: append([]string(nil), selected...), Checks: append([]Check(nil), checks...)}
	for _, path := range paths {
		before, err := fingerprintFile(manifest.RepoRoot, path)
		if err != nil {
			return err
		}
		after, err := fingerprintFile(manifest.WorktreePath, path)
		if err != nil {
			return err
		}
		journal.Files = append(journal.Files, IntegrationFile{Path: path, Before: before, After: after})
	}
	manifest.Integration = journal
	return m.persist(*manifest)
}

// recoverIntegration never repeats a prepared effect. Even when all files look
// like the preimage, a competing external edit could be indistinguishable from
// an unapplied patch; an explicit reconciliation is required instead.
func (m *Manager) recoverIntegration(ctx context.Context, handle Handle, selected []string) (Result, error) {
	manifest := handle.Manifest
	journal := manifest.Integration
	if journal == nil || !slices.Equal(journal.Selected, selected) || journal.ArtifactID == "" || len(journal.Files) == 0 {
		return Result{}, fmt.Errorf("integration journal does not match the requested patch")
	}
	if _, err := m.LoadArtifact(ctx, journal.ArtifactID); err != nil {
		return Result{}, fmt.Errorf("load retained integration artifact: %w", err)
	}
	result := Result{TaskID: manifest.TaskID, AttemptID: manifest.AttemptID, BaseRevision: manifest.BaseRevision,
		FinalHash: journal.ArtifactID, ArtifactID: journal.ArtifactID, ReceiptID: manifest.ID, Cleanup: Cleanup{State: string(CleanupPending), ManifestPath: manifest.ManifestPath}}
	for _, file := range journal.Files {
		result.ChangedPaths = append(result.ChangedPaths, file.Path)
		if err := validateRelativePath(file.Path); err != nil {
			return result, err
		}
		current, err := fingerprintFile(manifest.RepoRoot, file.Path)
		if err != nil {
			return result, fmt.Errorf("inspect integration result for %q: %w", file.Path, err)
		}
		if current != file.After {
			return result, fmt.Errorf("integration result for %q is ambiguous; retained for reconciliation", file.Path)
		}
	}
	if journal.State != "applied" {
		if journal.State != "prepared" {
			return result, fmt.Errorf("unknown integration state %q", journal.State)
		}
		journal.State = "applied"
		if err := m.persist(manifest); err != nil {
			return result, fmt.Errorf("persist observed integration: %w", err)
		}
	}
	if err := m.retainReceipt(manifest); err != nil {
		return result, fmt.Errorf("retain observed integration receipt: %w", err)
	}
	if err := m.cleanup(ctx, handle); err != nil {
		return result, fmt.Errorf("integration applied but cleanup pending: %w", err)
	}
	result.Cleanup = Cleanup{State: "complete"}
	return result, nil
}
