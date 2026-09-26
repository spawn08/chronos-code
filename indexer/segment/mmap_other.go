//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package segment

// mapFile reads the whole file on platforms without the mmap path. Segments
// are small (about 1 MiB per 500 Go files), so this costs one read at open.
func mapFile(path string) ([]byte, func() error, error) { return readWhole(path) }
