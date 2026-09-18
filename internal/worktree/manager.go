// Package worktree manages crash-recoverable, project-private git worktrees.
package worktree

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type DirtyPolicy string

const (
	DirtyReject   DirtyPolicy = "reject"
	DirtyPreserve DirtyPolicy = "preserve"
)

type CreateOptions struct {
	TaskID      string
	AttemptID   string
	DirtyPolicy DirtyPolicy
}

type Handle struct{ Manifest Manifest }

type Manager struct {
	dataDir string
	runner  Runner
	now     func() time.Time
	random  func([]byte) error
	mu      sync.Mutex
	active  map[string]Handle
}

func New(dataDir string, runner Runner) (*Manager, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("project data directory is required")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve project data directory: %w", err)
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Manager{dataDir: filepath.Clean(abs), runner: runner, now: time.Now, active: make(map[string]Handle), random: func(p []byte) error {
		_, err := rand.Read(p)
		return err
	}}, nil
}

// Create captures the canonical parent state and creates a worktree from HEAD only.
// DirtyPreserve records parent changes but deliberately does not copy them.
func (m *Manager) Create(ctx context.Context, repo string, options CreateOptions) (Handle, error) {
	if options.TaskID == "" || options.AttemptID == "" {
		return Handle{}, fmt.Errorf("task ID and attempt ID are required")
	}
	if options.DirtyPolicy == "" {
		options.DirtyPolicy = DirtyReject
	}
	if options.DirtyPolicy != DirtyReject && options.DirtyPolicy != DirtyPreserve {
		return Handle{}, fmt.Errorf("unsupported dirty policy %q", options.DirtyPolicy)
	}
	rootResult, err := m.git(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return Handle{}, fmt.Errorf("resolve repository root: %w", err)
	}
	root := strings.TrimRight(string(rootResult.Stdout), "\r\n")
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Handle{}, fmt.Errorf("canonicalize repository root: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Handle{}, fmt.Errorf("resolve canonical repository root: %w", err)
	}
	head, err := m.git(ctx, root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Handle{}, fmt.Errorf("resolve base HEAD: %w", err)
	}
	base := strings.TrimSpace(string(head.Stdout))
	dirtyResult, err := m.git(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return Handle{}, fmt.Errorf("capture parent dirty state: %w", err)
	}
	dirty, err := parseStatusPaths(dirtyResult.Stdout)
	if err != nil {
		return Handle{}, fmt.Errorf("capture parent dirty state: %w", err)
	}
	if options.DirtyPolicy == DirtyReject && len(dirty) > 0 {
		return Handle{}, fmt.Errorf("parent worktree is dirty (%d paths); use preserve policy to isolate from it", len(dirty))
	}

	manifest, err := m.allocate(options, root, base, dirty)
	if err != nil {
		return Handle{}, err
	}
	if err := m.persist(manifest); err != nil {
		return Handle{}, err
	}
	if err := os.MkdirAll(filepath.Dir(manifest.WorktreePath), 0o700); err != nil {
		return Handle{}, fmt.Errorf("create private worktree directory: %w", err)
	}
	_, err = m.git(ctx, root, "worktree", "add", "-b", strings.TrimPrefix(manifest.Ref, "refs/heads/"), manifest.WorktreePath, manifest.BaseRevision)
	if err != nil {
		manifest.CleanupState = CleanupPending
		_ = m.persist(manifest)
		return Handle{Manifest: manifest}, fmt.Errorf("create isolated worktree (recovery manifest retained): %w", err)
	}
	manifest.CleanupState = CleanupActive
	if err := m.persist(manifest); err != nil {
		return Handle{Manifest: manifest}, fmt.Errorf("activate worktree manifest: %w", err)
	}
	handle := Handle{Manifest: manifest}
	m.mu.Lock()
	m.active[manifest.ID] = handle
	m.mu.Unlock()
	return handle, nil
}

func (m *Manager) allocate(options CreateOptions, root, base string, dirty []string) (Manifest, error) {
	for i := 0; i < 16; i++ {
		buf := make([]byte, 16)
		if err := m.random(buf); err != nil {
			return Manifest{}, fmt.Errorf("allocate worktree identity: %w", err)
		}
		id := hex.EncodeToString(buf)
		manifestPath := filepath.Join(m.manifestDir(), id+".json")
		worktreePath := filepath.Join(m.dataDir, "worktrees", "checkouts", id)
		if _, err := os.Lstat(manifestPath); err == nil || !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if _, err := os.Lstat(worktreePath); err == nil || !errors.Is(err, os.ErrNotExist) {
			continue
		}
		return Manifest{
			Version: manifestVersion, ID: id, TaskID: options.TaskID, AttemptID: options.AttemptID,
			RepoRoot: root, BaseRevision: base, DirtyPolicy: options.DirtyPolicy,
			ParentDirtyPaths: append([]string(nil), dirty...), WorktreePath: worktreePath,
			Ref:          "refs/heads/chronos-code/" + safeID(options.TaskID) + "/" + safeID(options.AttemptID) + "/" + id,
			CleanupState: CleanupPrepared, CreatedAt: m.now().UTC(), ManifestPath: manifestPath,
		}, nil
	}
	return Manifest{}, fmt.Errorf("could not allocate a collision-free worktree identity")
}

func (m *Manager) git(ctx context.Context, dir string, args ...string) (CommandResult, error) {
	return m.runner.Run(ctx, Command{Dir: dir, Args: args})
}

// Recover returns all valid, project-private manifests left by live or crashed attempts.
func (m *Manager) Recover() ([]Handle, error) {
	entries, err := os.ReadDir(m.manifestDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan worktree manifests: %w", err)
	}
	var handles []Handle
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		manifest, err := readManifest(filepath.Join(m.manifestDir(), entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("recover %s: %w", entry.Name(), err)
		}
		if !pathWithin(filepath.Join(m.dataDir, "worktrees", "checkouts"), manifest.WorktreePath) {
			return nil, fmt.Errorf("recover %s: worktree path escapes project data directory", entry.Name())
		}
		handles = append(handles, Handle{Manifest: manifest})
	}
	sort.Slice(handles, func(i, j int) bool { return handles[i].Manifest.CreatedAt.Before(handles[j].Manifest.CreatedAt) })
	return handles, nil
}

func (m *Manager) Remove(ctx context.Context, handle Handle) error {
	return m.cleanup(ctx, handle)
}

func (m *Manager) Cancel(ctx context.Context, handle Handle) error {
	return m.cleanup(ctx, handle)
}

// Close removes worktrees created by this manager instance. Cleanup failures
// leave their manifests in place for a later recovery pass.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	handles := make([]Handle, 0, len(m.active))
	for _, handle := range m.active {
		handles = append(handles, handle)
	}
	m.mu.Unlock()
	var errs []error
	for _, handle := range handles {
		if err := m.cleanup(ctx, handle); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) cleanup(ctx context.Context, handle Handle) error {
	manifest := handle.Manifest
	if err := m.validateOwned(manifest); err != nil {
		return err
	}
	manifest.CleanupState = CleanupPending
	if err := m.persist(manifest); err != nil {
		return err
	}
	var cleanupErr error
	if _, err := m.git(ctx, manifest.RepoRoot, "worktree", "remove", "--force", manifest.WorktreePath); err != nil && fileExists(manifest.WorktreePath) {
		cleanupErr = err
	}
	if _, err := m.git(ctx, manifest.RepoRoot, "update-ref", "-d", manifest.Ref); err != nil && refExists(ctx, m, manifest) {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("cleanup worktree (manifest retained): %w", cleanupErr)
	}
	if err := os.Remove(manifest.ManifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove worktree manifest: %w", err)
	}
	m.mu.Lock()
	delete(m.active, manifest.ID)
	m.mu.Unlock()
	return nil
}

// Prune asks git to clear stale administrative entries, then finishes cleanup-pending manifests.
func (m *Manager) Prune(ctx context.Context) error {
	handles, err := m.Recover()
	if err != nil {
		return err
	}
	repos := make(map[string]struct{})
	for _, handle := range handles {
		repos[handle.Manifest.RepoRoot] = struct{}{}
	}
	for repo := range repos {
		if _, err := m.git(ctx, repo, "worktree", "prune"); err != nil {
			return fmt.Errorf("prune git worktrees: %w", err)
		}
	}
	for _, handle := range handles {
		if handle.Manifest.CleanupState == CleanupPending || !fileExists(handle.Manifest.WorktreePath) {
			if err := m.cleanup(ctx, handle); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) validateOwned(manifest Manifest) error {
	expectedManifest := filepath.Join(m.manifestDir(), manifest.ID+".json")
	expectedWorktree := filepath.Join(m.dataDir, "worktrees", "checkouts", manifest.ID)
	expectedRef := "refs/heads/chronos-code/" + safeID(manifest.TaskID) + "/" + safeID(manifest.AttemptID) + "/" + manifest.ID
	if manifest.ManifestPath != expectedManifest || manifest.WorktreePath != expectedWorktree || manifest.Ref != expectedRef ||
		manifest.ID == "" || manifest.RepoRoot == "" {
		return fmt.Errorf("worktree is not owned by this project data directory")
	}
	return nil
}

func refExists(ctx context.Context, m *Manager, manifest Manifest) bool {
	_, err := m.git(ctx, manifest.RepoRoot, "show-ref", "--verify", "--quiet", manifest.Ref)
	return err == nil
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func parseStatusPaths(data []byte) ([]string, error) {
	parts := strings.Split(string(data), "\x00")
	paths := make(map[string]struct{})
	for i := 0; i < len(parts)-1; i++ {
		record := parts[i]
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("invalid porcelain status record")
		}
		paths[record[3:]] = struct{}{}
		if record[0] == 'R' || record[0] == 'C' || record[1] == 'R' || record[1] == 'C' {
			i++
			if i >= len(parts)-1 || parts[i] == "" {
				return nil, fmt.Errorf("invalid rename status record")
			}
			paths[parts[i]] = struct{}{}
		}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}
