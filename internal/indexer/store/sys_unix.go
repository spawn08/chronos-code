//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func lockDir(path string) (func() error, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open index lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock index: %w", err)
	}
	return func() error {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return f.Close()
	}, nil
}

// syncFile uses plain fsync. On darwin, os.File.Sync issues F_FULLFSYNC,
// which costs tens of milliseconds per call; the index is a rebuildable cache
// whose segments are checksummed, so a lost or torn generation after power
// loss is detected at open and rebuilt.
func syncFile(f *os.File) error { return syscall.Fsync(int(f.Fd())) }

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return syscall.Fsync(int(d.Fd()))
}
