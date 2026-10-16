package security

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spawn08/chronos/storage"
)

// ReadLog resolves a host-created log by session-owned ID, never by a supplied
// path. Logs remain readable after a shell is forgotten or the host restarts.
func (b *BackgroundShells) ReadLog(ctx context.Context, id string) ([]byte, error) {
	if !strings.HasPrefix(id, "sh-") || len(id) > 64 || strings.ContainsAny(id, "/\\.\x00") {
		return nil, fmt.Errorf("invalid shell artifact ID")
	}
	owner := fmt.Sprintf("%x", sha256.Sum256([]byte(storage.TenantFromContext(ctx)+"\x00"+storage.SessionFromContext(ctx))))
	path := filepath.Join(b.dir, owner, id+".log")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("shell artifact unavailable in this session")
	}
	if info.Size() > 32<<20 {
		return nil, fmt.Errorf("shell artifact exceeds 32 MiB; narrow the command output")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read shell artifact: %w", err)
	}
	return data, nil
}

// ReadLogChunk provides bounded access at any byte offset, including logs larger
// than the line reader's scan limit. A negative offset is relative to EOF.
func (b *BackgroundShells) ReadLogChunk(ctx context.Context, id string, offset int64, limit int) ([]byte, int64, error) {
	if !strings.HasPrefix(id, "sh-") || len(id) > 64 || strings.ContainsAny(id, "/\\.\x00") || limit < 1 || limit > 32768 {
		return nil, 0, fmt.Errorf("invalid shell artifact range")
	}
	owner := fmt.Sprintf("%x", sha256.Sum256([]byte(storage.TenantFromContext(ctx)+"\x00"+storage.SessionFromContext(ctx))))
	path := filepath.Join(b.dir, owner, id+".log")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("shell artifact unavailable in this session")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, 0, fmt.Errorf("shell artifact changed while opening")
	}
	if offset < 0 {
		offset = max(0, info.Size()+offset)
	}
	if offset < 0 || offset > info.Size() {
		return nil, 0, fmt.Errorf("offset outside shell artifact")
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)))
	return data, offset, err
}
