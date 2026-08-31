package tui

import "strings"

// History is readline-style command history for the interactive REPL: Up/Down
// recall of previously submitted messages, plus a Ctrl+R substring search.
// It holds no bubbletea state so it's unit-testable independent of the TUI.
type History struct {
	entries []string
	cursor  int    // index into entries during recall; len(entries) means "not recalling"
	draft   string // in-progress input saved when recall starts, restored by Next past the newest entry
}

// NewHistory returns an empty History ready for use.
func NewHistory() *History {
	return &History{}
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
	}
	h.Reset()
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
