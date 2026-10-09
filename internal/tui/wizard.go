package tui

import (
	"fmt"
	"net/url"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/spawn08/chronos-code/internal/auth"
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

// wizardField is one prompt of stepTextInput. Most providers need only an
// API key; Azure also needs its endpoint, deployment, and API version.
type wizardField struct {
	title       string
	placeholder string
	password    bool
	optional    bool
	validate    func(string) error
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
	fields   []wizardField
	answers  []string
	err      string
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
		wizardItem{label: "Sign in with ChatGPT / Codex subscription", hint: "opens your browser · OpenAI Codex / ChatGPT", value: subscriptionLoginValue},
		wizardItem{label: "Use an API key", hint: "Anthropic, OpenAI, Azure, Gemini, …", value: "apikey"},
		wizardItem{label: "Enterprise OAuth (your IdP)", hint: "requires your own registered OAuth app", value: "oauth"},
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

// loginFields lists the text prompts stepTextInput asks for method and
// provider, in order.
func loginFields(method, provider string) []wizardField {
	if method != "apikey" {
		return []wizardField{{
			title:       fmt.Sprintf("Enter OAuth config for %s (client-id auth-url token-url):", provider),
			placeholder: "client-id auth-url token-url",
		}}
	}
	apiKey := wizardField{title: fmt.Sprintf("Enter API key for %s:", provider), placeholder: fmt.Sprintf("API key for %s", provider), password: true}
	if provider != "azure" {
		return []wizardField{apiKey}
	}
	apiKey.title = "Azure OpenAI API key (2/4):"
	return []wizardField{
		{title: "Azure OpenAI endpoint (1/4):", placeholder: "https://<resource>.openai.azure.com", validate: validateEndpoint},
		apiKey,
		{title: "Deployment name (3/4):", placeholder: "e.g. gpt-4o · optional, choose later with /model azure <deployment>", optional: true},
		{title: "API version (4/4):", placeholder: "optional · default 2024-10-21", optional: true},
	}
}

func validateEndpoint(v string) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("endpoint must be a URL such as https://<resource>.openai.azure.com")
	}
	return nil
}

// startField focuses a fresh text input for the field at len(w.answers).
func (w *loginWizard) startField() tea.Cmd {
	f := w.fields[len(w.answers)]
	w.input = newTextPrompt(f.placeholder, f.password)
	w.err = ""
	return textinput.Blink
}

// title returns the wizard's current step heading, for renderWizardModal.
func (w *loginWizard) title() string {
	switch w.step {
	case stepProvider:
		return "Select provider to configure:"
	case stepTextInput:
		return w.fields[len(w.answers)].title
	default:
		return "Select authentication method:"
	}
}

// View renders the wizard's current step body (list picker or text input).
func (w *loginWizard) View() string {
	if w.step == stepTextInput {
		view := w.input.View()
		if w.err != "" {
			view += "\n" + styleError.Render(w.err)
		}
		hint := "enter submit  esc cancel"
		if w.fields[len(w.answers)].optional {
			hint = "enter submit (blank to skip)  esc cancel"
		}
		return view + "\n" + styleDim.Render(hint)
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
		w.fields = loginFields(w.method, w.provider)
		w.answers = nil
		return m, w.startField()
	}
	return m, nil
}

// updateWizardInput forwards a non-key message (bracketed paste, the
// textinput's own clipboard paste result, cursor blink) to the wizard's
// text input. It reports false when no text-input step is active.
func (m *appModel) updateWizardInput(msg tea.Msg) (tea.Cmd, bool) {
	if m.wizard == nil || m.wizard.step != stepTextInput {
		return nil, false
	}
	var cmd tea.Cmd
	m.wizard.input, cmd = m.wizard.input.Update(msg)
	return cmd, true
}

// submitWizardInput records the current field's answer and moves to the next
// field. After the last one it closes the wizard and hands the answers to
// handleLoginCommand — the exact same parsing/execution path the typed
// "/login <provider> ..." form uses, so the wizard is purely a friendlier
// way to compose that command, not a second implementation of login. Azure
// goes through handleAzureLogin, since its settings have no typed form.
func (m *appModel) submitWizardInput() (tea.Model, tea.Cmd) {
	w := m.wizard
	line := strings.TrimSpace(w.input.Value())
	field := w.fields[len(w.answers)]
	if line == "" && !field.optional {
		if len(w.fields) == 1 {
			m.wizard = nil
		}
		return m, nil
	}
	if line != "" && field.validate != nil {
		if err := field.validate(line); err != nil {
			w.err = err.Error()
			return m, nil
		}
	}
	w.answers = append(w.answers, line)
	if len(w.answers) < len(w.fields) {
		return m, w.startField()
	}
	m.wizard = nil

	var cmd tea.Cmd
	switch {
	case w.method == "apikey" && w.provider == "azure":
		m.handleAzureLogin(w.answers[1], auth.AzureSettings{Endpoint: w.answers[0], Deployment: w.answers[2], APIVersion: w.answers[3]})
	case w.method == "oauth":
		cmd = m.handleLoginCommand(w.provider + " oauth " + line)
	default:
		cmd = m.handleLoginCommand(w.provider + " " + line)
	}
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
