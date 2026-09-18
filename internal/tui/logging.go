package tui

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

const (
	maxTUILogBytes   = 2 << 20
	maxTUILogBackups = 3
)

// Bubble Tea owns the terminal while the alternate screen is active. The
// standard logger (also used by the default slog handler) must write elsewhere,
// or background watcher messages invalidate the renderer's screen contents.
func redirectTUILogs() (func(), error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("locate TUI log directory: %w", err)
	}
	dir := filepath.Join(cache, "chronos-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create TUI log directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure TUI log directory: %w", err)
	}
	path := filepath.Join(dir, "tui.log")
	w, err := openRotatingLog(path, maxTUILogBytes, maxTUILogBackups)
	if err != nil {
		return nil, err
	}
	previous := log.Writer()
	log.SetOutput(w)
	return func() {
		log.SetOutput(previous)
		_ = w.Close()
	}, nil
}

type rotatingLogWriter struct {
	mu      sync.Mutex
	path    string
	max     int64
	backups int
	file    *os.File
	size    int64
}

func openRotatingLog(path string, maxBytes int64, backups int) (*rotatingLogWriter, error) {
	if err := rotateTUILog(path, maxBytes, backups); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open TUI log: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secure TUI log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("inspect TUI log: %w", err)
	}
	return &rotatingLogWriter{path: path, max: maxBytes, backups: backups, file: f, size: info.Size()}, nil
}

func (w *rotatingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	originalLen := len(p)
	if w.max > 0 && int64(len(p)) > w.max {
		p = p[len(p)-int(w.max):]
	}
	if w.max > 0 && w.size > 0 && w.size+int64(len(p)) > w.max {
		if err := w.file.Close(); err != nil {
			return 0, err
		}
		if err := rotateTUILog(w.path, 1, w.backups); err != nil {
			return 0, err
		}
		file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return 0, err
		}
		w.file, w.size = file, 0
	}
	if _, err := w.file.Write(p); err != nil {
		return 0, err
	}
	w.size += int64(len(p))
	return originalLen, nil
}

func (w *rotatingLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

func rotateTUILog(path string, maxBytes int64, backups int) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect TUI log: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("TUI log is not a regular file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure TUI log: %w", err)
	}
	if maxBytes <= 0 || info.Size() < maxBytes {
		return nil
	}
	for i := backups - 1; i >= 1; i-- {
		older := fmt.Sprintf("%s.%d", path, i)
		newer := fmt.Sprintf("%s.%d", path, i+1)
		if oldInfo, statErr := os.Lstat(older); statErr == nil && !oldInfo.Mode().IsRegular() {
			return fmt.Errorf("TUI log backup is not a regular file")
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect TUI log backup: %w", statErr)
		}
		if i == backups-1 {
			_ = os.Remove(newer)
		}
		if err := os.Rename(older, newer); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate TUI log: %w", err)
		} else if err == nil {
			if err := os.Chmod(newer, 0o600); err != nil {
				return fmt.Errorf("secure rotated TUI log: %w", err)
			}
		}
	}
	if backups > 0 {
		if err := os.Rename(path, path+".1"); err != nil {
			return fmt.Errorf("rotate TUI log: %w", err)
		}
		if err := os.Chmod(path+".1", 0o600); err != nil {
			return fmt.Errorf("secure rotated TUI log: %w", err)
		}
		return nil
	}
	return os.Remove(path)
}
