package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/spawn08/chronos-code/internal/modelinfo"
)

// wizardStep enumerates /login's interactive flow (bound to the reuse-only
// decision on subscription auth: chronos-code has no OAuth client of its
// own, so "use a subscription" only ever means "reuse an already-detected
// Claude Code/Codex CLI login" — never a fresh sign-in this tool can't
// actually perform).
type wizardStep int

const (
	stepAuthMethod wizardStep = iota
	stepProvider
	stepTextInput
)

// wizardItem is one selectable row in a list-picker step.
type wizardItem struct {
	label string
	hint  string
	value string
}

// loginWizard drives /login's interactive picker. It is nil on appModel
// whenever no wizard is in progress.
type loginWizard struct {
	step     wizardStep
	method   string // "apikey" or "oauth", set once stepAuthMethod is answered
	provider string
	items    []wizardItem
	idx      int
	input    textinput.Model
}

// subscriptionLoginValue is the wizard item value that triggers the
// OpenAI/ChatGPT subscription browser login (see advanceWizard). It's not
// offered for Anthropic: Anthropic blocked third-party subscription OAuth
// by ToS in February 2026 and technically shut it off in April 2026, so
// chronos-code only ever offers Anthropic auth via API key or reusing an
// existing Claude Code login.
const subscriptionLoginValue = "openai-subscription"

// newLoginWizard builds stepAuthMethod's item list: one "use existing
// login" entry per DetectedExternalLogins result, the OpenAI subscription
// browser flow, then the two paths chronos-code can always offer
// regardless of what's already installed.
func newLoginWizard(m *appModel) *loginWizard {
	w := &loginWizard{step: stepAuthMethod}
	for _, d := range m.orch.DetectedExternalLogins() {
		w.items = append(w.items, wizardItem{label: "Use existing login", hint: d.Label, value: "existing:" + d.Provider})
	}
	w.items = append(w.items,
		wizardItem{label: "Sign in with ChatGPT subscription", hint: "opens your browser · OpenAI only, see /help", value: subscriptionLoginValue},
		wizardItem{label: "Use an API key", value: "apikey"},
		wizardItem{label: "Custom OAuth (enterprise IdP)", hint: "requires your own registered OAuth app", value: "oauth"},
	)
	return w
}

// providerItems lists every provider modelinfo knows about, deduplicated,
// for stepProvider. It's a starting point, not an exhaustive list — a user
// needing a provider outside this set can still use the typed form
// (/login <provider> <api-key>) directly.
func providerItems() []wizardItem {
	seen := make(map[string]bool)
	var items []wizardItem
	for _, i := range modelinfo.All() {
		if seen[i.Provider] {
			continue
		}
		seen[i.Provider] = true
		items = append(items, wizardItem{label: i.Provider, value: i.Provider})
	}
	return items
}

func newTextPrompt(placeholder string, password bool) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.Focus()
	ti.CharLimit = 512
	ti.SetWidth(60)
	if password {
		ti.EchoMode = textinput.EchoPassword
	}
	return ti
}

// title returns the wizard's current step heading, for renderWizardModal.
func (w *loginWizard) title() string {
	switch w.step {
	case stepProvider:
		return "Select provider to configure:"
	case stepTextInput:
		if w.method == "apikey" {
			return fmt.Sprintf("Enter API key for %s:", w.provider)
		}
		return fmt.Sprintf("Enter OAuth config for %s (client-id auth-url token-url):", w.provider)
	default:
		return "Select authentication method:"
	}
}

// View renders the wizard's current step body (list picker or text input).
func (w *loginWizard) View() string {
	if w.step == stepTextInput {
		return w.input.View()
	}
	var b strings.Builder
	for i, it := range w.items {
		marker := "  "
		if i == w.idx {
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

// handleWizardKey routes a key event while a wizard is active, mirroring
// handleApprovalKey/handleSearchKey's modal-takes-all-input pattern.
func (m *appModel) handleWizardKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	defer m.resizeViewport()
	w := m.wizard
	if w.step == stepTextInput {
		switch msg.Code {
		case tea.KeyEsc:
			m.wizard = nil
			return m, nil
		case tea.KeyEnter:
			return m.submitWizardInput()
		}
		var cmd tea.Cmd
		w.input, cmd = w.input.Update(msg)
		return m, cmd
	}

	switch msg.Code {
	case tea.KeyEsc:
		m.wizard = nil
		return m, nil
	case tea.KeyUp:
		if w.idx > 0 {
			w.idx--
		}
		return m, nil
	case tea.KeyDown:
		if w.idx < len(w.items)-1 {
			w.idx++
		}
		return m, nil
	case tea.KeyEnter:
		if len(w.items) == 0 {
			return m, nil
		}
		return m.advanceWizard(w.items[w.idx].value)
	}
	return m, nil
}

// advanceWizard handles a list-picker selection: either it's terminal
// (the "existing login" and "ChatGPT subscription" branches don't need a
// provider/text-input step), or it moves to the next step.
func (m *appModel) advanceWizard(value string) (tea.Model, tea.Cmd) {
	w := m.wizard
	switch w.step {
	case stepAuthMethod:
		if provider, ok := strings.CutPrefix(value, "existing:"); ok {
			m.wizard = nil
			m.handleWhoamiCommand(provider)
			m.viewport.SetContent(m.renderTranscript())
			m.viewport.GotoBottom()
			return m, nil
		}
		if value == subscriptionLoginValue {
			m.wizard = nil
			cmd := m.startSubscriptionLogin()
			m.viewport.SetContent(m.renderTranscript())
			m.viewport.GotoBottom()
			return m, cmd
		}
		w.method = value
		w.step = stepProvider
		w.items = providerItems()
		w.idx = 0
		return m, nil

	case stepProvider:
		w.provider = value
		w.step = stepTextInput
		if w.method == "apikey" {
			w.input = newTextPrompt(fmt.Sprintf("API key for %s", w.provider), true)
		} else {
			w.input = newTextPrompt("client-id auth-url token-url", false)
		}
		return m, textinput.Blink
	}
	return m, nil
}

// submitWizardInput closes the wizard and hands its collected answer to
// handleLoginCommand — the exact same parsing/execution path the typed
// "/login <provider> ..." form uses, so the wizard is purely a friendlier
// way to compose that command, not a second implementation of login.
func (m *appModel) submitWizardInput() (tea.Model, tea.Cmd) {
	w := m.wizard
	line := strings.TrimSpace(w.input.Value())
	m.wizard = nil
	if line == "" {
		return m, nil
	}

	arg := w.provider + " " + line
	if w.method == "oauth" {
		arg = w.provider + " oauth " + line
	}
	cmd := m.handleLoginCommand(arg)
	m.viewport.SetContent(m.renderTranscript())
	m.viewport.GotoBottom()
	return m, cmd
}

// renderWizardModal renders the active wizard step as a bordered modal,
// matching renderApprovalModal's chrome.
func (m *appModel) renderWizardModal() string {
	body := styleHeader.Render(m.wizard.title()) + "\n\n" + m.wizard.View()
	width := m.width - inputBoxBorderWidth
	if width < 1 {
		width = 1
	}
	return styleModal.Width(width).Render(body)
}
