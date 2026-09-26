//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package store

import (
	"fmt"
	"os"
)

// lockDir uses an exclusive-create lock file where flock is unavailable. On
// Windows a file held open by a live process cannot be removed, so a stale
// file left by a crashed process is removed and retried while a live holder
// yields ErrLocked. Elsewhere this is best effort; LockFileEx is future work.
func lockDir(path string) (func() error, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			if rmErr := os.Remove(path); rmErr != nil {
				return nil, ErrLocked // held open by a live process
			}
			return lockDir(path)
		}
		return nil, fmt.Errorf("lock index: %w", err)
	}
	return func() error {
		f.Close()
		return os.Remove(path)
	}, nil
}

func syncFile(f *os.File) error { return f.Sync() }

func syncDir(string) error { return nil }
