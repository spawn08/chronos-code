package retention

import (
	"context"
	"errors"
	"os"
)

// ReadOnlyFileAdapter inventories a store that cannot be safely pruned by the
// generic engine. Scoped domain APIs must perform its deletion.
type ReadOnlyFileAdapter struct{ Name, Path string }

func (a ReadOnlyFileAdapter) Scope() string { return a.Name }
func (a ReadOnlyFileAdapter) Limitation() string {
	return "use cleanup prune plan_db with an explicit tenant and repository"
}
func (a ReadOnlyFileAdapter) Inventory(context.Context) ([]Item, error) {
	info, err := os.Lstat(a.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	return []Item{{Scope: a.Name, Key: a.Path, UpdatedAt: info.ModTime().UTC(), Bytes: info.Size()}}, nil
}
func (a ReadOnlyFileAdapter) Delete(context.Context, []Item) error {
	return errors.New("resource requires its scoped transactional prune API")
}
