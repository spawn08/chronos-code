package tui

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	f, err := os.OpenFile(filepath.Join(dir, "tui.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open TUI log: %w", err)
	}
	previous := log.Writer()
	log.SetOutput(f)
	return func() {
		log.SetOutput(previous)
		_ = f.Close()
	}, nil
}
