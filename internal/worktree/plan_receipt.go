package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WithVerifiedPlanReceipt reconciles a prepared-but-applied integration without
// reapplying its patch. The integration lock stays held through the plan-store
// acceptance callback so another managed integration cannot change the parent
// between observation and the node transition.
func (m *Manager) WithVerifiedPlanReceipt(ctx context.Context, repo, receiptID, taskID, attemptID string, accept func(IntegrationReceipt) error) error {
	if accept == nil || taskID == "" || attemptID == "" {
		return fmt.Errorf("plan receipt requires an attempt and acceptance callback")
	}
	var err error
	repo, err = filepath.EvalSymlinks(repo)
	if err != nil {
		return fmt.Errorf("resolve plan receipt repository: %w", err)
	}
	unlock, err := m.lockIntegration(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	receipt, err := m.LoadReceipt(receiptID)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := m.receiptPath(receiptID); err != nil {
			return err
		}
		manifest, manifestErr := readManifest(filepath.Join(m.manifestDir(), receiptID+".json"))
		if manifestErr != nil {
			return fmt.Errorf("load pending plan integration: %w", manifestErr)
		}
		if manifest.TaskID != taskID || manifest.AttemptID != attemptID || manifest.RepoRoot != repo || manifest.Integration == nil || len(manifest.Integration.Checks) != 1 || !manifest.Integration.Checks[0].Passed || manifest.Integration.Checks[0].Name != "plan-node-verification" {
			return fmt.Errorf("pending integration does not match verified plan attempt")
		}
		if _, err := m.recoverIntegration(ctx, Handle{Manifest: manifest}, manifest.Integration.Selected); err != nil {
			return fmt.Errorf("reconcile pending plan integration: %w", err)
		}
		receipt, err = m.LoadReceipt(receiptID)
	}
	if err != nil {
		return err
	}
	if receipt.State != "applied" || receipt.TaskID != taskID || receipt.AttemptID != attemptID || receipt.RepoRoot != repo ||
		len(receipt.Checks) != 1 || receipt.Checks[0].Name != "plan-node-verification" || !receipt.Checks[0].Passed {
		return fmt.Errorf("integration receipt does not prove this plan attempt")
	}
	root, err := m.git(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("integration receipt belongs to a different repository: %w", err)
	}
	if strings.TrimSpace(string(root.Stdout)) != receipt.RepoRoot {
		return fmt.Errorf("integration receipt belongs to a different repository")
	}
	head, err := m.git(ctx, repo, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("integration receipt parent revision changed: %w", err)
	}
	if strings.TrimSpace(string(head.Stdout)) != receipt.ParentRevision {
		return fmt.Errorf("integration receipt parent revision changed")
	}
	if _, err := m.LoadArtifact(ctx, receipt.ArtifactID); err != nil {
		return fmt.Errorf("verify retained patch: %w", err)
	}
	for _, file := range receipt.Files {
		current, err := fingerprintFile(receipt.RepoRoot, file.Path)
		if err != nil {
			return fmt.Errorf("integration path %q no longer matches the verified postimage: %w", file.Path, err)
		}
		if current != file.After {
			return fmt.Errorf("integration path %q no longer matches the verified postimage", file.Path)
		}
	}
	return accept(receipt)
}
