package tui

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/spawn08/chronos-code/internal/modelinfo"
	"github.com/spawn08/chronos/storage"
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

	// isModelPicker marks a picker built by newModelPicker, so a
	// modelPickerLiveMsg that arrives after the user has already dismissed
	// or replaced it (Ctrl+A, Ctrl+/, Esc) knows not to merge into whatever
	// picker (if any) is open by then.
	isModelPicker   bool
	isSessionPicker bool
	loading         bool
	more            bool
	offset          int
	agentID         string
	ctx             context.Context
	cancel          context.CancelFunc
	details         map[string]string

	// scrollOffset is the index into items of the first row View renders.
	// View recomputes it every render (see ensureVisible) to track idx, so
	// a long list — e.g. every live-fetched anthropic/openai/azure model
	// merged into the Ctrl+M picker — scrolls to keep the selection visible
	// instead of silently overflowing the terminal.
	scrollOffset int
}

const (
	sessionPageSize       = 100
	maxPickerSessions     = 1000
	maxSessionDetailBytes = 16 << 10
)

type sessionLister interface {
	List(context.Context, string, int, int) ([]*storage.Session, error)
}

type sessionPickerMsg struct {
	picker  *picker
	turnID  uint64
	items   []wizardItem
	details map[string]string
	count   int
	err     error
}

func (m *appModel) openSessionPicker() tea.Cmd {
	if m.picker != nil && m.picker.cancel != nil {
		m.picker.cancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.picker = &picker{heading: "Loading sessions…", filterable: true, isSessionPicker: true,
		ctx: ctx, cancel: cancel, agentID: m.orch.ActiveID(), details: make(map[string]string), more: true}
	cmd := loadSessionPageCmd(m.picker, m.turnID, m.orch.SessionManager())
	m.resizeViewport()
	return cmd
}

func loadSessionPageCmd(p *picker, turnID uint64, manager sessionLister) tea.Cmd {
	p.loading = true
	ctx, agentID, offset := p.ctx, p.agentID, p.offset
	return func() tea.Msg {
		msg := sessionPickerMsg{picker: p, turnID: turnID, details: make(map[string]string)}
		if err := ctx.Err(); err != nil {
			msg.err = err
			return msg
		}
		rows, err := manager.List(ctx, agentID, sessionPageSize, offset)
		if err != nil {
			msg.err = fmt.Errorf("load sessions: %w", err)
			return msg
		}
		rows = rows[:min(len(rows), sessionPageSize)]
		for _, row := range rows {
			if row == nil {
				continue
			}
			title, _ := row.Metadata["title"].(string)
			if len(title) > 256 {
				title = strings.ToValidUTF8(title[:256], "") + "…"
			}
			value := "/resume " + row.ID
			msg.items = append(msg.items, wizardItem{label: strings.TrimSpace(row.ID + " " + title),
				hint: row.Status + " · " + row.UpdatedAt.Format("2006-01-02 15:04"), value: value})
			detail := inspectionValue(row)
			if len(detail) > maxSessionDetailBytes {
				detail = detail[:maxSessionDetailBytes]
				for n := 0; n < utf8.UTFMax-1 && !utf8.ValidString(detail); n++ {
					detail = detail[:len(detail)-1]
				}
				detail += "\n[session metadata capped at 16 KiB]"
			}
			msg.details[value] = detail
		}
		msg.count = len(rows)
		msg.err = ctx.Err()
		return msg
	}
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
// when it can't reach a provider's live models endpoint. This keeps opening
// the picker instant rather than blocking Update() on a network call; the
// Ctrl+M handler separately kicks off fetchModelPickerLiveCmd, and a
// modelPickerLiveMsg upgrades these entries for the active provider in
// place once that live fetch lands (see mergeLiveModelPickerItems).
func newModelPicker(m *appModel) *picker {
	authorized := m.authorizedProviderNames()
	list := filterByProviders(modelinfo.All(), authorized)
	var items []wizardItem
	for _, i := range list {
		items = append(items, modelPickerItem(i, false))
	}
	heading := "Switch model:"
	if len(items) == 0 {
		heading = "Switch model (no provider authorized yet — run /login):"
	}
	return &picker{heading: heading, all: items, items: items, isModelPicker: true, filterable: true}
}

func modelPickerItem(i modelinfo.Info, live bool) wizardItem {
	hint := formatTokenCount(i.ContextWindow) + " tokens"
	if live {
		hint += " · live"
	}
	return wizardItem{
		label: fmt.Sprintf("%s / %s", i.Provider, i.Model),
		hint:  hint,
		value: fmt.Sprintf("/model %s %s", i.Provider, i.Model),
	}
}

// mergeLiveModelPickerItems replaces items' static entries for provider
// with live (real, vendor-API-confirmed model IDs), preserving their
// position — this is how e.g. a live Azure deployment list overwrites the
// three generic example deployment names newModelPicker started with.
// Provider entries with no live counterpart are left untouched.
func mergeLiveModelPickerItems(items []wizardItem, provider string, live []modelinfo.Info) []wizardItem {
	prefix := "/model " + provider + " "
	liveItems := make([]wizardItem, len(live))
	for i, info := range live {
		liveItems[i] = modelPickerItem(info, true)
	}

	out := make([]wizardItem, 0, len(items)+len(liveItems))
	spliced := false
	for _, it := range items {
		if !strings.HasPrefix(it.value, prefix) {
			out = append(out, it)
			continue
		}
		if !spliced {
			out = append(out, liveItems...)
			spliced = true
		}
	}
	if !spliced {
		out = append(out, liveItems...)
	}
	return out
}

// paletteCommands lists every slash command documented in helpText, in the
// same order, for Ctrl+/'s fuzzy-filtered palette.
var paletteCommands = []string{
	"/agents", "/agent", "/model", "/think", "/login", "/logout", "/whoami",
	"/context", "/usage", "/stream", "/session", "/resume", "/compact", "/rewind", "/plan", "/learn", "/sandbox", "/memory", "/budget", "/workspace",
	"/skills", "/mcp", "/subagent", "/copy", "/mouse", "/clear", "/perf", "/help", "/quit",
	"/session list", "/inspect",
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
		p.scrollOffset = 0
		return
	}
	needle := strings.ToLower(p.filter)
	items := make([]wizardItem, 0, len(p.all))
	for _, it := range p.all {
		search := it.label
		if p.isSessionPicker {
			search += " " + it.hint
		}
		if strings.Contains(strings.ToLower(search), needle) {
			items = append(items, it)
		}
	}
	p.items = items
	p.idx = 0
	p.scrollOffset = 0
}

// ensureVisible scrolls the window (scrollOffset) by the minimum amount
// needed to keep idx inside a window of visible rows, clamped to the item
// list's bounds. Called from View on every render, so it stays correct
// regardless of what changed idx or items (navigation, filtering, or a
// modelPickerLiveMsg merge growing the list).
func (p *picker) ensureVisible(visible int) {
	if visible <= 0 {
		p.scrollOffset = 0
		return
	}
	if p.idx < p.scrollOffset {
		p.scrollOffset = p.idx
	}
	if p.idx >= p.scrollOffset+visible {
		p.scrollOffset = p.idx - visible + 1
	}
	maxOffset := len(p.items) - visible
	if maxOffset < 0 {
		maxOffset = 0
	}
	if p.scrollOffset > maxOffset {
		p.scrollOffset = maxOffset
	}
	if p.scrollOffset < 0 {
		p.scrollOffset = 0
	}
}

// View renders the picker's heading, optional filter line, and a
// vertically scrolled window of up to visible items — sized by the caller
// to fit the terminal (see appModel.pickerVisibleRows) — so a long list
// (e.g. every live-fetched anthropic/openai/azure model merged into the
// Ctrl+M picker) scrolls with the selection instead of overflowing.
func (p *picker) View(visible int) string {
	if visible <= 0 || visible > len(p.items) {
		visible = len(p.items)
	}
	p.ensureVisible(visible)

	var b strings.Builder
	b.WriteString(styleHeader.Render(p.heading))
	b.WriteString("\n")
	if p.filterable {
		b.WriteString(styleDim.Render("filter: ") + p.filter + "▏\n")
	}
	b.WriteString("\n")
	if len(p.items) == 0 {
		label := "  (no matches)"
		if p.isSessionPicker && p.loading {
			label = "  (loading…)"
		} else if p.isSessionPicker && p.filter == "" {
			label = "  (no sessions)"
		}
		b.WriteString(styleDim.Render(label))
	}
	end := p.scrollOffset + visible
	if end > len(p.items) {
		end = len(p.items)
	}
	for i := p.scrollOffset; i < end; i++ {
		it := p.items[i]
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
	footer := "↑↓ navigate  enter select  esc cancel"
	if p.isSessionPicker {
		footer = "↑↓ enter resume · tab info · esc"
		if p.loading {
			footer += "\nloading sessions…"
		} else if p.more {
			footer += "\nctrl+n more · filter loaded sessions"
		}
	}
	if len(p.items) > visible {
		footer = fmt.Sprintf("showing %d-%d of %d · %s", p.scrollOffset+1, end, len(p.items), footer)
	}
	b.WriteString(styleDim.Render("\n" + footer))
	return strings.TrimRight(b.String(), "\n")
}

// renderPickerModal renders the active picker as a bordered modal,
// matching renderWizardModal/renderApprovalModal's chrome.
func (m *appModel) renderPickerModal() string {
	width := m.width
	if width < 1 {
		width = 1
	}
	return styleModal.Width(width).Render(truncateToWidth(m.picker.View(m.pickerVisibleRows()), max(1, width-6)))
}

// pickerVisibleRows caps how many items picker.View renders at once so a
// long list never overflows the terminal. Heading, the blank line before
// and after the list, styleModal's border+padding, header/status and the
// empty transcript row consume 11 rows (12 with a filter line) — the same
// fixed-chrome-budget pattern as approvalDetailBudget for the approval
// modal.
func (m *appModel) pickerVisibleRows() int {
	chrome := 11
	if m.picker != nil && m.picker.filterable {
		chrome++
	}
	if m.picker != nil && m.picker.isSessionPicker {
		chrome++
	}
	budget := m.height - chrome
	return max(1, budget)
}

// handlePickerKey routes a key event while a picker is active, mirroring
// handleWizardKey/handleSearchKey's modal-takes-all-input pattern.
func (m *appModel) handlePickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	defer m.resizeViewport()
	p := m.picker
	if p.isSessionPicker && msg.String() == "ctrl+n" && p.more && !p.loading {
		return m, loadSessionPageCmd(p, m.turnID, m.orch.SessionManager())
	}
	switch msg.Code {
	case tea.KeyEsc:
		if p.cancel != nil {
			p.cancel()
		}
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
	case tea.KeyTab:
		if p.isSessionPicker && len(p.items) > 0 {
			m.openInspection("Session · read-only", p.details[p.items[p.idx].value])
		}
		return m, nil
	case tea.KeyEnter:
		if len(p.items) == 0 {
			return m, nil
		}
		value := p.items[p.idx].value
		if p.isSessionPicker {
			if m.sending {
				m.statusMsg = "finish or interrupt the active turn before resuming"
				return m, nil
			}
			p.cancel()
			m.picker = nil
			orch, id := m.orch, strings.TrimPrefix(value, "/resume ")
			return m, m.maintenanceCmd("resuming session", func(ctx context.Context) (string, error) {
				resumed, err := orch.ResumeSession(ctx, id)
				return "resumed session " + resumed, err
			})
		}
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
