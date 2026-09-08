package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/spawn08/chronos-code/internal/modelinfo"
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

func TestMergeLiveModelPickerItems_ReplacesProviderInPlace(t *testing.T) {
	items := []wizardItem{
		{label: "anthropic / claude-sonnet-5", value: "/model anthropic claude-sonnet-5"},
		{label: "azure / gpt-4o", value: "/model azure gpt-4o"},
		{label: "azure / gpt-5", value: "/model azure gpt-5"},
		{label: "openai / gpt-5", value: "/model openai gpt-5"},
	}

	got := mergeLiveModelPickerItems(items, "azure", []modelinfo.Info{
		{Provider: "azure", Model: "my-real-deployment"},
	})

	want := []string{
		"/model anthropic claude-sonnet-5",
		"/model azure my-real-deployment",
		"/model openai gpt-5",
	}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d; got = %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].value != w {
			t.Errorf("got[%d].value = %q, want %q", i, got[i].value, w)
		}
	}
	if !strings.Contains(got[1].hint, "live") {
		t.Errorf("live entry hint = %q, want it to indicate live", got[1].hint)
	}
}

func TestMergeLiveModelPickerItems_AppendsWhenProviderAbsent(t *testing.T) {
	items := []wizardItem{{label: "openai / gpt-5", value: "/model openai gpt-5"}}

	got := mergeLiveModelPickerItems(items, "azure", []modelinfo.Info{
		{Provider: "azure", Model: "my-real-deployment"},
	})

	if len(got) != 2 || got[1].value != "/model azure my-real-deployment" {
		t.Fatalf("got = %+v, want openai entry followed by the appended azure entry", got)
	}
}

func TestPicker_EnsureVisibleScrollsWithSelection(t *testing.T) {
	items := make([]wizardItem, 10)
	for i := range items {
		items[i] = wizardItem{label: fmt.Sprintf("item-%d", i), value: fmt.Sprintf("/item-%d", i)}
	}
	p := &picker{all: items, items: items}

	// Rendering with a 3-row window should keep idx 0 at the top.
	p.View(3)
	if p.scrollOffset != 0 {
		t.Fatalf("scrollOffset = %d, want 0 for idx 0", p.scrollOffset)
	}

	// Moving past the bottom of the window must scroll down by the minimum
	// amount needed to keep idx visible.
	p.idx = 7
	p.View(3)
	if p.scrollOffset != 5 {
		t.Fatalf("scrollOffset = %d, want 5 (idx 7 visible in a 3-row window)", p.scrollOffset)
	}

	// Moving back above the window must scroll back up to idx.
	p.idx = 1
	p.View(3)
	if p.scrollOffset != 1 {
		t.Fatalf("scrollOffset = %d, want 1 (idx 1 visible in a 3-row window)", p.scrollOffset)
	}
}

func TestPicker_ViewCapsRenderedItemsToVisible(t *testing.T) {
	items := make([]wizardItem, 10)
	for i := range items {
		items[i] = wizardItem{label: fmt.Sprintf("item-%d", i), value: fmt.Sprintf("/item-%d", i)}
	}
	p := &picker{all: items, items: items}

	out := p.View(3)
	rendered := 0
	for i := range items {
		if strings.Contains(out, items[i].label) {
			rendered++
		}
	}
	if rendered != 3 {
		t.Fatalf("rendered %d items in output, want exactly 3 (the visible window)\n%s", rendered, out)
	}
	if !strings.Contains(out, "showing 1-3 of 10") {
		t.Fatalf("View output missing scroll indicator: %s", out)
	}
}

func TestNewModelPicker_IsFilterable(t *testing.T) {
	m := newTestAppModel(t)
	p := newModelPicker(m)
	if !p.filterable {
		t.Error("newModelPicker() picker must be filterable (searchable), like newCommandPalette()")
	}
}

func TestHandlePickerKey_Navigation(t *testing.T) {
	p := &picker{items: []wizardItem{{label: "a", value: "/a"}, {label: "b", value: "/b"}, {label: "c", value: "/c"}}}
	m := &appModel{picker: p}

	_, _ = m.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.idx != 1 {
		t.Fatalf("idx after KeyDown = %d, want 1", p.idx)
	}
	_, _ = m.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyDown})
	_, _ = m.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyDown}) // at bottom, should not overflow
	if p.idx != 2 {
		t.Fatalf("idx after 3x KeyDown on a 3-item list = %d, want 2 (clamped)", p.idx)
	}
	_, _ = m.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if p.idx != 1 {
		t.Fatalf("idx after KeyUp = %d, want 1", p.idx)
	}

	_, _ = m.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.picker != nil {
		t.Error("picker should be nil after Esc")
	}
}

func TestHandlePickerKey_FilterTyping(t *testing.T) {
	p := newCommandPalette()
	m := &appModel{picker: p}

	_, _ = m.handlePickerKey(tea.KeyPressMsg{Code: 'm', Text: "mo"})
	if p.filter != "mo" {
		t.Fatalf("filter = %q, want %q", p.filter, "mo")
	}
	if len(p.items) == 0 {
		t.Fatal("expected filtered items for \"mo\"")
	}

	_, _ = m.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if p.filter != "m" {
		t.Fatalf("filter after backspace = %q, want %q", p.filter, "m")
	}
}
