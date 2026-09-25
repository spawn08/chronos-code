package worktree

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// IntegrationReceipt survives worktree cleanup. Undo affects only its selected
// file patch; shell/API effects and dependent plan nodes are not reversed.
type IntegrationReceipt struct {
	ID             string            `json:"id"`
	TaskID         string            `json:"task_id,omitempty"`
	AttemptID      string            `json:"attempt_id,omitempty"`
	RepoRoot       string            `json:"repo_root"`
	ParentRevision string            `json:"parent_revision"`
	ArtifactID     string            `json:"artifact_id"`
	State          string            `json:"state"`
	Files          []IntegrationFile `json:"files"`
	Checks         []Check           `json:"checks,omitempty"`
}

func (m *Manager) receiptPath(id string) (string, error) {
	if len(id) != 32 {
		return "", fmt.Errorf("invalid integration receipt identity")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", fmt.Errorf("invalid integration receipt identity: %w", err)
	}
	return filepath.Join(m.dataDir, "artifacts", "receipts", id+".json"), nil
}

func (m *Manager) retainReceipt(manifest Manifest) error {
	journal := manifest.Integration
	if journal == nil || journal.State != "applied" || journal.ArtifactID == "" || len(journal.Files) == 0 {
		return fmt.Errorf("cannot retain an unobserved integration")
	}
	revision := manifest.ParentRevision
	if revision == "" {
		revision = manifest.BaseRevision
	}
	receipt := IntegrationReceipt{ID: manifest.ID, TaskID: manifest.TaskID, AttemptID: manifest.AttemptID, RepoRoot: manifest.RepoRoot, ParentRevision: revision,
		ArtifactID: journal.ArtifactID, State: "applied", Files: append([]IntegrationFile(nil), journal.Files...), Checks: append([]Check(nil), journal.Checks...)}
	if existing, err := m.LoadReceipt(receipt.ID); err == nil {
		if existing.ID != receipt.ID || existing.TaskID != receipt.TaskID || existing.AttemptID != receipt.AttemptID || existing.RepoRoot != receipt.RepoRoot || existing.ParentRevision != receipt.ParentRevision || existing.ArtifactID != receipt.ArtifactID || !sameReceiptFiles(existing.Files, receipt.Files) || !slices.Equal(existing.Checks, receipt.Checks) {
			return fmt.Errorf("integration receipt conflicts with existing artifact")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return m.writeReceipt(receipt)
}

func sameReceiptFiles(first, second []IntegrationFile) bool {
	if len(first) != len(second) {
		return false
	}
	for i := range first {
		if first[i] != second[i] {
			return false
		}
	}
	return true
}

func (m *Manager) LoadReceipt(id string) (IntegrationReceipt, error) {
	path, err := m.receiptPath(id)
	if err != nil {
		return IntegrationReceipt{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return IntegrationReceipt{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return IntegrationReceipt{}, fmt.Errorf("integration receipt must be a bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return IntegrationReceipt{}, fmt.Errorf("read integration receipt: %w", err)
	}
	var receipt IntegrationReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return IntegrationReceipt{}, fmt.Errorf("decode integration receipt: %w", err)
	}
	if receipt.ID != id || receipt.RepoRoot == "" || receipt.ParentRevision == "" || receipt.ArtifactID == "" || len(receipt.Files) == 0 ||
		(receipt.State != "applied" && receipt.State != "undo_prepared" && receipt.State != "undone") {
		return IntegrationReceipt{}, fmt.Errorf("invalid integration receipt")
	}
	for _, file := range receipt.Files {
		if err := validateRelativePath(file.Path); err != nil {
			return IntegrationReceipt{}, err
		}
	}
	return receipt, nil
}

func (m *Manager) writeReceipt(receipt IntegrationReceipt) error {
	path, err := m.receiptPath(receipt.ID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode integration receipt: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create integration receipt directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return fmt.Errorf("create integration receipt: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
		if err == nil {
			err = tmp.Sync()
		}
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write integration receipt: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("publish integration receipt: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("sync integration receipt directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync integration receipt directory: %w", err)
	}
	return nil
}

// Undo reverses an accepted file slice only when the parent still matches its
// recorded postimage and branch revision. It cannot undo external effects or
// dependent artifacts, which must be reconciled separately by the caller.
func (m *Manager) Undo(ctx context.Context, repo, receiptID string) error {
	unlock, err := m.lockIntegration(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	receipt, err := m.LoadReceipt(receiptID)
	if err != nil {
		return err
	}
	root, err := m.git(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("resolve undo repository: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(strings.TrimSpace(string(root.Stdout)))
	if err != nil || canonical != receipt.RepoRoot {
		return fmt.Errorf("undo receipt does not belong to this repository")
	}
	head, err := m.git(ctx, canonical, "rev-parse", "--verify", "HEAD")
	if err != nil || strings.TrimSpace(string(head.Stdout)) != receipt.ParentRevision {
		return fmt.Errorf("undo parent revision changed")
	}
	patch, err := m.LoadArtifact(ctx, receipt.ArtifactID)
	if err != nil {
		return fmt.Errorf("load undo artifact: %w", err)
	}
	post, pre := true, true
	for _, file := range receipt.Files {
		if err := rejectSymlinkTraversal(canonical, file.Path); err != nil {
			return err
		}
		current, err := fingerprintFile(canonical, file.Path)
		if err != nil {
			return err
		}
		post = post && current == file.After
		pre = pre && current == file.Before
	}
	if receipt.State == "undone" {
		if pre {
			return nil
		}
		return fmt.Errorf("undone slice was subsequently modified")
	}
	if receipt.State == "undo_prepared" && pre {
		receipt.State = "undone"
		return m.writeReceipt(receipt)
	}
	if !post {
		return fmt.Errorf("undo slice conflicts with parent edits; no files changed")
	}
	if receipt.State == "applied" {
		receipt.State = "undo_prepared"
		if err := m.writeReceipt(receipt); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"apply", "-R", "--check", "--binary", "--whitespace=nowarn", "-"}, {"apply", "-R", "--binary", "--whitespace=nowarn", "-"}} {
		if _, err := m.runner.Run(ctx, Command{Dir: canonical, Args: args, Stdin: patch}); err != nil {
			return fmt.Errorf("reverse accepted slice: %w", err)
		}
	}
	for _, file := range receipt.Files {
		current, err := fingerprintFile(canonical, file.Path)
		if err != nil || current != file.Before {
			return fmt.Errorf("undo result for %q needs reconciliation", file.Path)
		}
	}
	receipt.State = "undone"
	return m.writeReceipt(receipt)
}
