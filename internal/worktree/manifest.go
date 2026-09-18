package worktree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const manifestVersion = 1

type CleanupState string

const (
	CleanupPrepared CleanupState = "prepared"
	CleanupActive   CleanupState = "active"
	CleanupPending  CleanupState = "cleanup_pending"
)

// Manifest is the durable recovery record written before a worktree is created.
type Manifest struct {
	Version          int          `json:"version"`
	ID               string       `json:"id"`
	TaskID           string       `json:"task_id"`
	AttemptID        string       `json:"attempt_id"`
	RepoRoot         string       `json:"repo_root"`
	BaseRevision     string       `json:"base_revision"`
	DirtyPolicy      DirtyPolicy  `json:"dirty_policy"`
	ParentDirtyPaths []string     `json:"parent_dirty_paths,omitempty"`
	WorktreePath     string       `json:"worktree_path"`
	Ref              string       `json:"ref"`
	CleanupState     CleanupState `json:"cleanup_state"`
	CreatedAt        time.Time    `json:"created_at"`
	ManifestPath     string       `json:"-"`
}

func (m *Manager) manifestDir() string { return filepath.Join(m.dataDir, "worktrees", "manifests") }

func (m *Manager) persist(manifest Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode worktree manifest: %w", err)
	}
	dir := filepath.Dir(manifest.ManifestPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create private manifest directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure manifest directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return fmt.Errorf("create worktree manifest: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmpName, manifest.ManifestPath)
	}
	if err == nil {
		if directory, openErr := os.Open(dir); openErr != nil {
			err = openErr
		} else {
			err = directory.Sync()
			if closeErr := directory.Close(); err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		return fmt.Errorf("persist worktree manifest: %w", err)
	}
	return nil
}

func readManifest(path string) (Manifest, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Manifest{}, err
	}
	if !info.Mode().IsRegular() {
		return Manifest{}, fmt.Errorf("manifest is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	manifest.ManifestPath = path
	if manifest.Version != manifestVersion || manifest.ID == "" || manifest.RepoRoot == "" ||
		manifest.WorktreePath == "" || manifest.Ref == "" || manifest.BaseRevision == "" {
		return Manifest{}, fmt.Errorf("invalid worktree manifest")
	}
	if filepath.Base(path) != manifest.ID+".json" {
		return Manifest{}, fmt.Errorf("manifest identity does not match filename")
	}
	return manifest, nil
}

func safeID(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
		if b.Len() == 40 {
			break
		}
	}
	value = strings.Trim(b.String(), ".")
	if value == "" {
		return "unknown"
	}
	return value
}
