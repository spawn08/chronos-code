package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestNewCommandPalette(t *testing.T) {
	p := newCommandPalette()
	if !p.filterable {
		t.Error("newCommandPalette() picker should be filterable")
	}
	if len(p.items) != len(paletteCommands) {
		t.Fatalf("len(items) = %d, want %d", len(p.items), len(paletteCommands))
	}
	if p.items[0].value != paletteCommands[0] {
		t.Errorf("items[0].value = %q, want %q", p.items[0].value, paletteCommands[0])
	}
}

func TestPicker_ApplyFilter(t *testing.T) {
	p := newCommandPalette()
	p.idx = 3 // simulate a prior selection before filtering

	p.filter = "mo"
	p.applyFilter()

	if p.idx != 0 {
		t.Errorf("idx = %d after filtering, want reset to 0", p.idx)
	}
	if len(p.items) == 0 {
		t.Fatal("expected at least one match for filter \"mo\" (/model)")
	}
	for _, it := range p.items {
		if !strings.Contains(strings.ToLower(it.label), "mo") {
			t.Errorf("item %q does not match filter %q", it.label, p.filter)
		}
	}

	p.filter = "zzz-no-such-command"
	p.applyFilter()
	if len(p.items) != 0 {
		t.Errorf("len(items) = %d, want 0 for a filter with no matches", len(p.items))
	}

	p.filter = ""
	p.applyFilter()
	if len(p.items) != len(p.all) {
		t.Errorf("len(items) = %d after clearing filter, want %d (all items restored)", len(p.items), len(p.all))
	}
}

func TestHandlePickerKey_Navigation(t *testing.T) {
	p := &picker{items: []wizardItem{{label: "a", value: "/a"}, {label: "b", value: "/b"}, {label: "c", value: "/c"}}}
	m := &appModel{picker: p}

	_, _ = m.handlePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	if p.idx != 1 {
		t.Fatalf("idx after KeyDown = %d, want 1", p.idx)
	}
	_, _ = m.handlePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handlePickerKey(tea.KeyMsg{Type: tea.KeyDown}) // at bottom, should not overflow
	if p.idx != 2 {
		t.Fatalf("idx after 3x KeyDown on a 3-item list = %d, want 2 (clamped)", p.idx)
	}
	_, _ = m.handlePickerKey(tea.KeyMsg{Type: tea.KeyUp})
	if p.idx != 1 {
		t.Fatalf("idx after KeyUp = %d, want 1", p.idx)
	}

	_, _ = m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.picker != nil {
		t.Error("picker should be nil after Esc")
	}
}

func TestHandlePickerKey_FilterTyping(t *testing.T) {
	p := newCommandPalette()
	m := &appModel{picker: p}

	_, _ = m.handlePickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("mo")})
	if p.filter != "mo" {
		t.Fatalf("filter = %q, want %q", p.filter, "mo")
	}
	if len(p.items) == 0 {
		t.Fatal("expected filtered items for \"mo\"")
	}

	_, _ = m.handlePickerKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if p.filter != "m" {
		t.Fatalf("filter after backspace = %q, want %q", p.filter, "m")
	}
}
