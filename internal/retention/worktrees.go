package retention

import (
	"context"
	"fmt"
	"os"

	"github.com/spawn08/chronos-code/internal/worktree"
)

// WorktreeAdapter inventories only cleanup-pending or already-missing
// worktrees. Active manifests are active resources and are never candidates.
type WorktreeAdapter struct {
	Manager *worktree.Manager
	handles map[string]worktree.Handle
}

func (a *WorktreeAdapter) Scope() string { return ScopeWorktrees }

func (a *WorktreeAdapter) Inventory(context.Context) ([]Item, error) {
	if a.Manager == nil {
		return nil, nil
	}
	handles, err := a.Manager.Recover()
	if err != nil {
		return nil, err
	}
	a.handles = make(map[string]worktree.Handle)
	var items []Item
	for _, handle := range handles {
		manifest := handle.Manifest
		_, statErr := os.Lstat(manifest.WorktreePath)
		if manifest.CleanupState != worktree.CleanupPending && !os.IsNotExist(statErr) {
			continue
		}
		var bytes int64
		if info, err := os.Lstat(manifest.ManifestPath); err == nil {
			bytes += info.Size()
		}
		a.handles[manifest.ID] = handle
		items = append(items, Item{Scope: ScopeWorktrees, Key: manifest.ID, UpdatedAt: manifest.CreatedAt.UTC(), Bytes: bytes})
	}
	return items, nil
}

func (a *WorktreeAdapter) Delete(ctx context.Context, items []Item) error {
	for _, item := range items {
		handle, ok := a.handles[item.Key]
		if !ok {
			continue
		}
		if err := a.Manager.Remove(ctx, handle); err != nil {
			return fmt.Errorf("remove worktree %s: %w", item.Key, err)
		}
	}
	return nil
}
