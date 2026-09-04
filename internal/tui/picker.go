package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/spawn08/chronos-code/internal/modelinfo"
)

// picker drives a single-step selectable overlay: the Ctrl+A agent picker,
// Ctrl+M model picker, and Ctrl+/ command palette. It reuses wizardItem and
// the same list-navigation feel as loginWizard's list steps, without the
// multi-step flow /login needs. Selecting an item dispatches its value as
// if the user had typed it and pressed Enter (handleSubmit) — the picker is
// purely a faster way to compose that input, not a second code path for
// switching agents/models or running a command.
type picker struct {
	heading    string
	all        []wizardItem // unfiltered source list; only the palette filters
	items      []wizardItem // currently visible (== all unless filtering)
	idx        int
	filter     string
	filterable bool
}

func newAgentPicker(m *appModel) *picker {
	var items []wizardItem
	activeID := m.orch.ActiveID()
	for _, id := range m.orch.ListAgents() {
		hint := ""
		if a, ok := m.orch.GetAgent(id); ok {
			hint = a.Name
		}
		label := id
		if id == m.orch.PrimaryID() {
			label += " (primary)"
		}
		if id == activeID {
			label += " (active)"
		}
		items = append(items, wizardItem{label: label, hint: hint, value: "/agent " + id})
	}
	return &picker{heading: "Switch agent:", all: items, items: items}
}

// newModelPicker lists the static model registry filtered to already
// authorized providers — the same fallback list handleModelCommand shows
// when it can't reach a provider's live models endpoint. Keeping the
// picker to the static list (rather than handleModelCommand's live-fetch
// path) keeps opening it instant rather than blocking on a network call.
func newModelPicker(m *appModel) *picker {
	authorized := m.orch.AuthorizedProviders(m.ctx, distinctProviders(modelinfo.All()))
	list := filterByProviders(modelinfo.All(), authorized)
	var items []wizardItem
	for _, i := range list {
		items = append(items, wizardItem{
			label: fmt.Sprintf("%s / %s", i.Provider, i.Model),
			hint:  formatTokenCount(i.ContextWindow) + " tokens",
			value: fmt.Sprintf("/model %s %s", i.Provider, i.Model),
		})
	}
	heading := "Switch model:"
	if len(items) == 0 {
		heading = "Switch model (no provider authorized yet — run /login):"
	}
	return &picker{heading: heading, all: items, items: items}
}

// paletteCommands lists every slash command documented in helpText, in the
// same order, for Ctrl+/'s fuzzy-filtered palette.
var paletteCommands = []string{
	"/agents", "/agent", "/model", "/think", "/login", "/logout", "/whoami",
	"/context", "/usage", "/stream", "/session", "/resume", "/compact", "/rewind", "/plan", "/learn", "/sandbox", "/memory", "/budget", "/workspace",
	"/skills", "/mcp", "/subagent", "/copy", "/mouse", "/clear", "/perf", "/help", "/quit",
}

func newCommandPalette() *picker {
	var items []wizardItem
	for _, c := range paletteCommands {
		items = append(items, wizardItem{label: c, value: c})
	}
	return &picker{heading: "Commands:", all: items, items: items, filterable: true}
}

// applyFilter recomputes items from all using filter as a case-insensitive
// substring match against each item's label, and resets idx so a previous
// selection index from a longer list can't point past the end.
func (p *picker) applyFilter() {
	if p.filter == "" {
		p.items = p.all
		p.idx = 0
		return
	}
	needle := strings.ToLower(p.filter)
	items := make([]wizardItem, 0, len(p.all))
	for _, it := range p.all {
		if strings.Contains(strings.ToLower(it.label), needle) {
			items = append(items, it)
		}
	}
	p.items = items
	p.idx = 0
}

// View renders the picker's heading, optional filter line, and item list.
func (p *picker) View() string {
	var b strings.Builder
	b.WriteString(styleHeader.Render(p.heading))
	b.WriteString("\n")
	if p.filterable {
		b.WriteString(styleDim.Render("filter: ") + p.filter + "▏\n")
	}
	b.WriteString("\n")
	if len(p.items) == 0 {
		b.WriteString(styleDim.Render("  (no matches)"))
	}
	for i, it := range p.items {
		marker := "  "
		if i == p.idx {
			marker = styleAgentName.Render("→ ")
		}
		line := marker + it.label
		if it.hint != "" {
			line += styleDim.Render(" · " + it.hint)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(styleDim.Render("\n↑↓ navigate  enter select  esc cancel"))
	return strings.TrimRight(b.String(), "\n")
}

// renderPickerModal renders the active picker as a bordered modal,
// matching renderWizardModal/renderApprovalModal's chrome.
func (m *appModel) renderPickerModal() string {
	width := m.width - inputBoxBorderWidth
	if width < 1 {
		width = 1
	}
	return styleModal.Width(width).Render(m.picker.View())
}

// handlePickerKey routes a key event while a picker is active, mirroring
// handleWizardKey/handleSearchKey's modal-takes-all-input pattern.
func (m *appModel) handlePickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	defer m.resizeViewport()
	p := m.picker
	switch msg.Code {
	case tea.KeyEsc:
		m.picker = nil
		return m, nil
	case tea.KeyUp:
		if p.idx > 0 {
			p.idx--
		}
		return m, nil
	case tea.KeyDown:
		if p.idx < len(p.items)-1 {
			p.idx++
		}
		return m, nil
	case tea.KeyEnter:
		if len(p.items) == 0 {
			return m, nil
		}
		value := p.items[p.idx].value
		m.picker = nil
		return m.handleSubmit(value)
	}
	if p.filterable {
		switch msg.Code {
		case tea.KeyBackspace:
			if len(p.filter) > 0 {
				p.filter = removeLastRune(p.filter)
				p.applyFilter()
			}
			return m, nil
		default:
			if msg.Text == "" {
				return m, nil
			}
			p.filter += msg.String()
			p.applyFilter()
			return m, nil
		}
	}
	return m, nil
}
