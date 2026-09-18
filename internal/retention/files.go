package retention

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const trashDirName = ".retention-trash"

type FileAdapter struct {
	Name           string
	Root           string
	ActiveKeys     map[string]bool
	ActivePrefixes []string
}

func (a *FileAdapter) Scope() string { return a.Name }

func (a *FileAdapter) Inventory(ctx context.Context) ([]Item, error) {
	root, err := secureRoot(a.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []Item
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == filepath.Join(root, trashDirName) {
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("retention path escapes data root: %s", path)
		}
		active := a.ActiveKeys[rel]
		for _, prefix := range a.ActivePrefixes {
			if rel == prefix || strings.HasPrefix(rel, prefix+string(filepath.Separator)) {
				active = true
				break
			}
		}
		if active {
			return nil
		}
		items = append(items, Item{Scope: a.Name, Key: filepath.ToSlash(rel), UpdatedAt: info.ModTime().UTC(), Bytes: info.Size()})
		return nil
	})
	return items, err
}

func (a *FileAdapter) Delete(ctx context.Context, items []Item) error {
	root, err := secureRoot(a.Root)
	if err != nil {
		return err
	}
	trash := filepath.Join(root, trashDirName)
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return fmt.Errorf("create retention trash: %w", err)
	}
	var errs []error
	for i, item := range items {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		path, err := containedRegular(root, item.Key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		tombstone := filepath.Join(trash, fmt.Sprintf("%d-%d", time.Now().UnixNano(), i))
		if err := os.Rename(path, tombstone); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if err := os.Remove(tombstone); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return joinErrors(errs)
}

// Recover removes rename-first tombstones left by interrupted cleanup. It is
// safe to call before adapters or databases are opened.
func Recover(dataRoot string, maxEntries int) error {
	if maxEntries <= 0 {
		maxEntries = 100
	}
	root, err := secureRoot(dataRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	seen := 0
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !entry.IsDir() || entry.Name() != trashDirName {
			return nil
		}
		children, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, child := range children {
			if seen >= maxEntries {
				break
			}
			seen++
			if child.Type()&os.ModeSymlink != 0 {
				continue
			}
			if err := os.RemoveAll(filepath.Join(path, child.Name())); err != nil {
				errs = append(errs, err)
			}
		}
		return filepath.SkipDir
	})
	return errors.Join(walkErr, joinErrors(errs))
}

func secureRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("retention root is not a real directory: %s", abs)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(canonical), nil
}

func containedRegular(root, key string) (string, error) {
	if key == "" || filepath.IsAbs(key) {
		return "", fmt.Errorf("invalid retention key %q", key)
	}
	path := filepath.Join(root, filepath.FromSlash(key))
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("retention path escapes data root: %q", key)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	if rel, err = filepath.Rel(root, parent); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("retention path resolves outside data root: %q", key)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("retention target is not a regular file: %q", key)
	}
	return path, nil
}
