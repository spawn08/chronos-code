package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxHistoryEntries = 500
	maxHistoryBytes   = 256 << 10
)

// History is readline-style command history for the interactive REPL: Up/Down
// recall of previously submitted messages, plus a Ctrl+R substring search.
// It holds no bubbletea state so it's unit-testable independent of the TUI.
type History struct {
	entries []string
	cursor  int    // index into entries during recall; len(entries) means "not recalling"
	draft   string // in-progress input saved when recall starts, restored by Next past the newest entry
	path    string
	limit   int
}

// NewHistory returns an empty History ready for use.
func NewHistory() *History {
	return &History{limit: maxHistoryEntries}
}

func newPersistentHistory() (*History, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("locate command history: %w", err)
	}
	return newHistoryAt(filepath.Join(cache, "chronos-code", "history.json"), maxHistoryEntries)
}

func newHistoryAt(path string, limit int) (*History, error) {
	if limit <= 0 {
		limit = maxHistoryEntries
	}
	h := &History{path: path, limit: limit}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return h, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect command history: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("command history is not a regular file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secure command history: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read command history: %w", err)
	}
	if len(data) > maxHistoryBytes {
		return nil, fmt.Errorf("command history exceeds %d bytes", maxHistoryBytes)
	}
	if err := json.Unmarshal(data, &h.entries); err != nil {
		return nil, fmt.Errorf("decode command history: %w", err)
	}
	h.bound()
	h.Reset()
	return h, nil
}

// Add records a submitted message and ends any in-progress recall. Blank
// entries and immediate repeats of the last entry are ignored, matching
// familiar shell history behavior.
func (h *History) Add(entry string) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return
	}
	if n := len(h.entries); n == 0 || h.entries[n-1] != entry {
		h.entries = append(h.entries, entry)
		h.bound()
		_ = h.persist()
	}
	h.Reset()
}

func (h *History) bound() {
	limit := h.limit
	if limit <= 0 {
		limit = maxHistoryEntries
	}
	if len(h.entries) > limit {
		h.entries = append([]string(nil), h.entries[len(h.entries)-limit:]...)
	}
}

func (h *History) persist() error {
	if h.path == "" {
		return nil
	}
	data, err := json.Marshal(h.entries)
	if err != nil {
		return fmt.Errorf("encode command history: %w", err)
	}
	for len(data) > maxHistoryBytes && len(h.entries) > 0 {
		h.entries = h.entries[1:]
		data, err = json.Marshal(h.entries)
		if err != nil {
			return fmt.Errorf("encode command history: %w", err)
		}
	}
	dir := filepath.Dir(h.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create private command history directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure command history directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".history-*")
	if err != nil {
		return fmt.Errorf("create command history: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, h.path)
	}
	if err != nil {
		return fmt.Errorf("persist command history: %w", err)
	}
	return nil
}

// Reset ends any in-progress recall, so a subsequent Prev starts from the
// newest entry again. Called when the user types instead of navigating, and
// after Add.
func (h *History) Reset() {
	h.cursor = len(h.entries)
	h.draft = ""
}

// Prev moves recall one step toward older entries. current is the text
// currently in the input box; it's captured as the draft the first time
// recall starts so a later Next can restore it. ok is false when already at
// the oldest entry (or there's no history at all).
func (h *History) Prev(current string) (text string, ok bool) {
	if len(h.entries) == 0 || h.cursor == 0 {
		return "", false
	}
	if h.cursor == len(h.entries) {
		h.draft = current
	}
	h.cursor--
	return h.entries[h.cursor], true
}

// Next moves recall one step toward newer entries, returning the draft (the
// text that was being typed before recall started) once past the newest
// entry. ok is false once already back at the draft with nothing newer to
// move to.
func (h *History) Next() (text string, ok bool) {
	if h.cursor >= len(h.entries) {
		return "", false
	}
	h.cursor++
	if h.cursor == len(h.entries) {
		return h.draft, true
	}
	return h.entries[h.cursor], true
}

// Search returns entries containing query (case-sensitive substring match),
// most-recently-submitted first, for Ctrl+R reverse search. An empty query
// matches nothing (an empty search box shouldn't dump the whole history).
func (h *History) Search(query string) []string {
	if query == "" {
		return nil
	}
	var out []string
	for i := len(h.entries) - 1; i >= 0; i-- {
		if strings.Contains(h.entries[i], query) {
			out = append(out, h.entries[i])
		}
	}
	return out
}
