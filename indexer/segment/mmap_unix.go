//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package segment

import (
	"fmt"
	"os"
	"syscall"
)

func mapFile(path string) ([]byte, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	size := info.Size()
	if size < headerSize || size > 1<<40 {
		return nil, nil, fmt.Errorf("%s: %w: size %d", path, ErrCorrupt, size)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return readWhole(path)
	}
	return data, func() error { return syscall.Munmap(data) }, nil
}
