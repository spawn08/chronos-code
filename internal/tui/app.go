package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"

	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/modelinfo"
	"github.com/spawn08/chronos-code/internal/orchestrator"
)

// Layout constants for the fixed chrome around the scrollback viewport: the
// one-line title bar, the input textarea's visible row count plus the
// rounded border it's wrapped in (top + bottom line, styleInputBox) and its
// horizontal padding (styleInputBox's Padding(0,1)), and the one-line status
// bar footer. inputBoxBorderWidth/inputBoxPaddingWidth mirror styleInputBox's
// own border/padding so the textarea's content width and the box's outer
// Width() call stay derived from the same numbers instead of separate magic
// constants that can drift out of sync.
const (
	headerHeight         = 1
	inputRows            = 2
	inputBoxBorderWidth  = 2 // rounded border, left + right
	inputBoxPaddingWidth = 2 // styleInputBox.Padding(0, 1), left + right
	statusHeight         = 1
)

// pendingApproval mirrors an in-flight approvalRequestMsg while the modal is
// shown; resolved by handleApprovalKey.
type pendingApproval struct {
	toolName string
	args     map[string]any
	resp     chan approvalDecision
}

// streamStartedMsg, streamDeltaMsg and streamDoneMsg drive the streaming
// path: orch.ChatStream's channel is read by a self-reissuing tea.Cmd
// (listenStream) rather than blocking Update, since Update must stay
// responsive to key events (including the approval modal) while a response
// streams in.
type streamStartedMsg struct {
	ch <-chan *model.ChatResponse
}

type streamDeltaMsg struct {
	resp *model.ChatResponse
	ch   <-chan *model.ChatResponse
}

type streamDoneMsg struct{}

// chatDoneMsg carries the result of a non-streaming orch.Chat call.
type chatDoneMsg struct {
	resp *model.ChatResponse
	err  error
}

type shellDoneMsg struct{ err error }

const frameTimingSamples = 100

type frameTiming struct {
	updateStart time.Time
	samples     [frameTimingSamples]time.Duration
	sampleIdx   int
	sampleCount int
}

func (ft *frameTiming) recordUpdateStart() { ft.updateStart = time.Now() }

func (ft *frameTiming) recordViewEnd() {
	if ft.updateStart.IsZero() {
		return
	}
	ft.samples[ft.sampleIdx] = time.Since(ft.updateStart)
	ft.sampleIdx = (ft.sampleIdx + 1) % frameTimingSamples
	if ft.sampleCount < frameTimingSamples {
		ft.sampleCount++
	}
	ft.updateStart = time.Time{}
}

func (ft *frameTiming) sorted() []time.Duration {
	if ft.sampleCount == 0 {
		return nil
	}
	s := make([]time.Duration, ft.sampleCount)
	copy(s, ft.samples[:ft.sampleCount])
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s
}

func (ft *frameTiming) percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func (ft *frameTiming) stats() string {
	s := ft.sorted()
	if len(s) == 0 {
		return "no frame timing data (send a message first)"
	}
	p50 := ft.percentile(s, 0.50)
	p95 := ft.percentile(s, 0.95)
	p99 := ft.percentile(s, 0.99)
	return fmt.Sprintf("frame timing (%d samples): p50=%s  p95=%s  p99=%s",
		len(s), p50, p95, p99)
}

// oauthEvent carries one step of an in-flight /login <provider> oauth flow:
// either the authorization URL (as soon as it's known, so the TUI can show
// it even if the automatic browser-open fails, e.g. over SSH) or the final
// outcome. oauthEventMsg re-arms listenOAuth after a non-final event, the
// same self-reissuing pattern streamDeltaMsg/listenStream use for chat
// streaming.
type oauthEvent struct {
	url  string
	done bool
	err  error
}

type oauthEventMsg struct {
	ev oauthEvent
	ch <-chan oauthEvent
}

// appModel is the interactive REPL's tea.Model. Pointer receivers throughout
// (rather than the copy-and-return style some bubbletea examples use) since
// several fields (viewport.Model, textarea.Model) carry meaningful internal
// state that's simpler to mutate in place.
type appModel struct {
	orch   *orchestrator.Orchestrator
	stream bool
	ctx    context.Context
	cancel context.CancelFunc

	viewport viewport.Model
	input    textarea.Model
	spin     spinner.Model
	history  *History

	width, height int
	workDir       string
	ready         bool

	blocks            []string // finalized, already-rendered transcript entries
	renderedBlocks    []string // cached rendered versions of m.blocks
	renderWidth       int      // width at which renderedBlocks were rendered
	transcriptBuf     strings.Builder
	activeAgentText   strings.Builder
	activeToolLines   []string
	lastChunk         string
	lastAssistantText string
	lastUsage         model.Usage
	// lastKnownUsage persists the most recent non-zero lastUsage across
	// turns (finalizeTurn zeroes lastUsage itself once each turn's status
	// line is computed), so /context and the status bar's context-usage
	// segment have something to show between turns, not just immediately
	// after one completes.
	lastKnownUsage model.Usage
	sending        bool

	statusMsg    string
	perf         frameTiming
	followOutput bool

	approval *pendingApproval
	wizard   *loginWizard
	picker   *picker

	// queuedMessage holds a message typed with Alt+Enter while a turn was
	// still streaming (m.sending); finalizeTurn dispatches it once that
	// turn completes, so the user doesn't have to wait to start typing the
	// next message.
	queuedMessage string

	searching     bool
	searchQuery   string
	searchResults []string
	searchIdx     int
	completionIdx int

	quitting bool
}

// RunTUI replaces the old bufio.Scanner-based REPL (NewREPL/Start) with a
// bubbletea program: scrollback viewport, multi-line input with history,
// markdown-lite response rendering, and a modal-based permission prompt that
// doesn't fight bubbletea for stdin the way a second bufio.Reader would.
func RunTUI(orch *orchestrator.Orchestrator, stream bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ta := textarea.New()
	ta.Placeholder = "Message chronos-code..."
	ta.Prompt = "❯ "
	ta.ShowLineNumbers = false
	ta.SetHeight(inputRows)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ta.Focus()

	wd, _ := os.Getwd()

	m := &appModel{
		orch:         orch,
		stream:       stream,
		ctx:          ctx,
		cancel:       cancel,
		input:        ta,
		spin:         spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		history:      NewHistory(),
		workDir:      wd,
		followOutput: true,
	}

	p := tea.NewProgram(m)
	installApprovalHandlers(orch, NewApprovalHandler(p))

	_, err := p.Run()
	return err
}

type approvalHandlerInstaller interface {
	SetApprovalHandler(tool.ApprovalFunc)
}

func installApprovalHandlers(installer approvalHandlerInstaller, handler tool.ApprovalFunc) {
	installer.SetApprovalHandler(handler)
}

// autoRemember best-effort extracts a standing instruction/correction from
// the user's message (PRD P2-002's heuristic auto-extraction) and saves it to
// the orchestrator's memory store. Errors and a disabled/nil memory store are
// both silently ignored — this is a background convenience, never something
// that should interrupt the conversation.
func autoRemember(orch *orchestrator.Orchestrator, message string) {
	store := orch.MemoryStore()
	if store == nil {
		return
	}
	if category, content, ok := memory.ExtractFromMessage(message); ok {
		_, _ = store.Add(category, content)
	}
}

func (m *appModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.input.Focus())
}

func (m *appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.perf.recordUpdateStart()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		vh := m.viewportHeight()
		if vh < 1 && m.approval == nil {
			vh = 1
		}
		if !m.ready {
			m.viewport = viewport.New()
			m.viewport.SetWidth(msg.Width)
			m.viewport.SetHeight(vh)
			m.ready = true
		} else {
			m.viewport.SetWidth(msg.Width)
			m.viewport.SetHeight(vh)
		}
		m.input.SetWidth(msg.Width - inputBoxBorderWidth - inputBoxPaddingWidth)
		m.refreshPrompt()
		m.viewport.SetContent(m.renderTranscript())
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case spinner.TickMsg:
		if !m.sending {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case approvalRequestMsg:
		m.approval = &pendingApproval{toolName: msg.toolName, args: msg.args, resp: msg.resp}
		m.resizeViewport()
		return m, nil

	case streamStartedMsg:
		return m, listenStream(msg.ch)

	case streamDeltaMsg:
		return m.handleStreamDelta(msg)

	case streamDoneMsg:
		return m, m.finalizeTurn(nil)

	case chatDoneMsg:
		if msg.resp != nil {
			for _, tc := range msg.resp.ToolCalls {
				m.activeToolLines = append(m.activeToolLines, RenderToolCall(tc.Name, SummarizeArgs(tc.Arguments)))
			}
			m.activeAgentText.WriteString(msg.resp.Content)
			m.lastUsage = msg.resp.Usage
		}
		return m, m.finalizeTurn(msg.err)

	case shellDoneMsg:
		if msg.err != nil {
			m.appendError(msg.err)
		}
		return m, nil

	case oauthEventMsg:
		return m.handleOAuthEvent(msg)
	}

	return m, nil
}

func (m *appModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, keys.Quit) {
		m.cancel()
		m.quitting = true
		return m, tea.Quit
	}
	if m.approval != nil {
		return m.handleApprovalKey(msg)
	}
	if m.wizard != nil {
		return m.handleWizardKey(msg)
	}
	if m.picker != nil {
		return m.handlePickerKey(msg)
	}
	if m.searching {
		return m.handleSearchKey(msg)
	}
	if m.ready && (msg.Code == tea.KeyPgUp || msg.Code == tea.KeyPgDown) {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		m.followOutput = m.viewport.AtBottom()
		return m, cmd
	}
	if completions := commandCompletions(m.input.Value()); len(completions) > 0 {
		if m.completionIdx >= len(completions) {
			m.completionIdx = 0
		}
		switch msg.Code {
		case tea.KeyTab:
			m.input.SetValue(completions[m.completionIdx])
			m.input.CursorEnd()
			m.completionIdx = 0
			m.resizeViewport()
			return m, nil
		case tea.KeyUp:
			m.completionIdx = (m.completionIdx - 1 + len(completions)) % len(completions)
			return m, nil
		case tea.KeyDown:
			m.completionIdx = (m.completionIdx + 1) % len(completions)
			return m, nil
		}
	}

	switch {
	case key.Matches(msg, keys.Submit):
		line := strings.TrimSpace(m.input.Value())
		if line == "" {
			return m, nil
		}
		m.input.Reset()
		m.completionIdx = 0
		m.resizeViewport()
		return m.handleSubmit(line)
	case msg.String() == "alt+enter" && m.sending:
		m.queuedMessage = m.input.Value()
		m.input.Reset()
		m.statusMsg = "queued — will send after the current turn finishes"
		return m, nil
	case key.Matches(msg, keys.AgentPicker):
		m.picker = newAgentPicker(m)
		return m, nil
	case key.Matches(msg, keys.ModelPicker):
		m.picker = newModelPicker(m)
		return m, nil
	case key.Matches(msg, keys.CommandPalette):
		m.picker = newCommandPalette()
		return m, nil
	case key.Matches(msg, keys.HistoryPrev):
		if !strings.Contains(m.input.Value(), "\n") {
			if v, ok := m.history.Prev(m.input.Value()); ok {
				m.input.SetValue(v)
				m.input.CursorEnd()
				return m, nil
			}
		}
	case key.Matches(msg, keys.HistoryNext):
		if !strings.Contains(m.input.Value(), "\n") {
			if v, ok := m.history.Next(); ok {
				m.input.SetValue(v)
				m.input.CursorEnd()
				return m, nil
			}
		}
	case key.Matches(msg, keys.ReverseSearch):
		m.searching = true
		m.searchQuery = ""
		m.updateSearchResults()
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	completions := commandCompletions(m.input.Value())
	if m.completionIdx >= len(completions) {
		m.completionIdx = 0
	}
	m.resizeViewport()
	return m, cmd
}

func (m *appModel) viewportHeight() int {
	bottomHeight := inputRows + inputBoxBorderWidth
	if m.approval != nil {
		bottomHeight = lipgloss.Height(m.renderApprovalModal())
	} else if len(commandCompletions(m.input.Value())) > 0 {
		bottomHeight++
	}
	height := m.height - headerHeight - bottomHeight - statusHeight
	if m.approval != nil && height < 0 {
		return 0
	}
	if m.approval == nil && height < 1 {
		return 1
	}
	return height
}

func (m *appModel) resizeViewport() {
	if m.ready {
		m.viewport.SetHeight(m.viewportHeight())
	}
}

func (m *appModel) handleApprovalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	a := m.approval
	switch msg.String() {
	case "y", "enter":
		a.resp <- approvalDecision{allow: true}
	case "a":
		a.resp <- approvalDecision{allow: true, always: true}
	case "A":
		a.resp <- approvalDecision{allow: true, all: true}
	case "n", "esc":
		a.resp <- approvalDecision{allow: false}
	default:
		return m, nil
	}
	m.approval = nil
	m.resizeViewport()
	return m, nil
}

func (m *appModel) handleSearchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.Code {
	case tea.KeyEsc:
		m.searching = false
		return m, nil
	case tea.KeyEnter:
		if len(m.searchResults) > 0 {
			m.input.SetValue(m.searchResults[m.searchIdx])
			m.input.CursorEnd()
		}
		m.searching = false
		return m, nil
	case tea.KeyUp:
		if len(m.searchResults) > 0 {
			m.searchIdx = (m.searchIdx + 1) % len(m.searchResults)
		}
		return m, nil
	case tea.KeyDown:
		if len(m.searchResults) > 0 {
			m.searchIdx = (m.searchIdx - 1 + len(m.searchResults)) % len(m.searchResults)
		}
		return m, nil
	case tea.KeyBackspace:
		if len(m.searchQuery) > 0 {
			m.searchQuery = m.searchQuery[:len(m.searchQuery)-1]
			m.updateSearchResults()
		}
		return m, nil
	default:
		if key.Matches(msg, keys.ReverseSearch) {
			if len(m.searchResults) > 0 {
				m.searchIdx = (m.searchIdx + 1) % len(m.searchResults)
			}
			return m, nil
		}
		if msg.Text == "" {
			return m, nil
		}
		m.searchQuery += msg.String()
		m.updateSearchResults()
		return m, nil
	}
}

func (m *appModel) updateSearchResults() {
	m.searchResults = m.history.Search(m.searchQuery)
	m.searchIdx = 0
}

func (m *appModel) handleSubmit(line string) (tea.Model, tea.Cmd) {
	switch {
	case strings.HasPrefix(line, "/"):
		return m.handleSlashCommand(line)
	case strings.HasPrefix(line, "!"):
		return m.handleShellEscape(line[1:])
	case strings.HasPrefix(line, "@"):
		parts := strings.SplitN(line[1:], " ", 2)
		if len(parts) == 2 {
			if err := m.orch.SwitchAgent(parts[0]); err != nil {
				m.appendError(err)
				return m, nil
			}
			line = parts[1]
		}
	}

	m.history.Add(line)
	autoRemember(m.orch, line)
	m.appendUserTurn(line)
	m.refreshPrompt()

	m.sending = true
	m.activeAgentText.Reset()
	m.activeToolLines = nil
	m.lastChunk = ""
	return m, tea.Batch(m.sendCmd(line), m.spin.Tick)
}

func (m *appModel) sendCmd(message string) tea.Cmd {
	orch := m.orch
	ctx := m.ctx
	stream := m.stream
	return func() tea.Msg {
		if stream {
			ch, err := orch.ChatStream(ctx, message)
			if err != nil {
				return chatDoneMsg{err: err}
			}
			return streamStartedMsg{ch: ch}
		}
		resp, err := orch.Chat(ctx, message)
		return chatDoneMsg{resp: resp, err: err}
	}
}

func listenStream(ch <-chan *model.ChatResponse) tea.Cmd {
	return func() tea.Msg {
		resp, ok := <-ch
		if !ok {
			return streamDoneMsg{}
		}
		return streamDeltaMsg{resp: resp, ch: ch}
	}
}

func (m *appModel) handleStreamDelta(msg streamDeltaMsg) (tea.Model, tea.Cmd) {
	resp := msg.resp
	if resp.Err != nil {
		return m, m.finalizeTurn(resp.Err)
	}
	if resp.Usage.PromptTokens > 0 {
		m.lastUsage = resp.Usage
	}
	if resp.Usage.CompletionTokens > m.lastUsage.CompletionTokens {
		m.lastUsage.CompletionTokens = resp.Usage.CompletionTokens
	}
	for _, tc := range resp.ToolCalls {
		m.activeToolLines = append(m.activeToolLines, RenderToolCall(tc.Name, SummarizeArgs(tc.Arguments)))
	}
	if resp.Content != "" && resp.Content != m.lastChunk {
		m.activeAgentText.WriteString(resp.Content)
		m.lastChunk = resp.Content
	}
	m.viewport.SetContent(m.renderTranscript())
	if m.followOutput {
		m.viewport.GotoBottom()
	}
	return m, listenStream(msg.ch)
}

func (m *appModel) handleShellEscape(cmdStr string) (tea.Model, tea.Cmd) {
	cmdStr = strings.TrimSpace(cmdStr)
	if cmdStr == "" {
		return m, nil
	}
	c := exec.CommandContext(m.ctx, "bash", "-c", cmdStr)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	return m, tea.ExecProcess(c, func(err error) tea.Msg {
		return shellDoneMsg{err: err}
	})
}

func (m *appModel) handleSlashCommand(line string) (tea.Model, tea.Cmd) {
	parts := strings.SplitN(line, " ", 2)
	cmd := strings.ToLower(parts[0])
	arg := ""
	if len(parts) > 1 {
		arg = strings.TrimSpace(parts[1])
	}

	switch cmd {
	case "/quit", "/exit", "/q":
		m.cancel()
		m.quitting = true
		return m, tea.Quit
	case "/help", "/h":
		m.appendSystem(helpText)
	case "/agents":
		var b strings.Builder
		for _, id := range m.orch.ListAgents() {
			marker := "  "
			if id == m.orch.ActiveID() {
				marker = "* "
			}
			a, _ := m.orch.GetAgent(id)
			fmt.Fprintf(&b, "%s%s — %s\n", marker, id, a.Name)
		}
		m.appendSystem(strings.TrimRight(b.String(), "\n"))
	case "/agent":
		if arg == "" {
			m.appendSystem(fmt.Sprintf("active: %s", m.orch.ActiveID()))
		} else if err := m.orch.SwitchAgent(arg); err != nil {
			m.appendError(err)
		} else {
			m.appendSystem(fmt.Sprintf("switched to %s", arg))
			m.refreshPrompt()
		}
	case "/model":
		m.handleModelCommand(arg)
	case "/login":
		if arg == "" {
			m.wizard = newLoginWizard(m)
			m.viewport.SetContent(m.renderTranscript())
			return m, nil
		}
		if cmd := m.handleLoginCommand(arg); cmd != nil {
			return m, cmd
		}
	case "/logout":
		if arg == "" {
			m.appendError(fmt.Errorf("usage: /logout <provider>"))
			break
		}
		if err := m.orch.Logout(arg); err != nil {
			m.appendError(err)
		} else {
			m.appendSystem(fmt.Sprintf("logged out of %q", arg))
		}
	case "/whoami":
		m.handleWhoamiCommand(arg)
	case "/context":
		m.handleContextCommand()
	case "/stream":
		m.stream = !m.stream
		m.appendSystem(fmt.Sprintf("streaming: %v", m.stream))
	case "/clear":
		if m.sending {
			m.appendError(fmt.Errorf("cannot clear context while a response is in progress"))
			break
		}
		if _, err := m.orch.ResetSession(m.ctx); err != nil {
			m.appendError(err)
			break
		}
		m.blocks = nil
		m.invalidateRenderCache()
		m.lastKnownUsage = model.Usage{}
		m.lastAssistantText = ""
		m.statusMsg = "new session started"
		m.followOutput = true
	case "/copy":
		if m.lastAssistantText == "" {
			m.appendError(fmt.Errorf("no assistant response to copy"))
			break
		}
		m.statusMsg = "copy requested"
		m.viewport.SetContent(m.renderTranscript())
		m.viewport.GotoBottom()
		return m, tea.SetClipboard(m.lastAssistantText)
	case "/perf":
		m.appendSystem(m.perf.stats())
	case "/session":
		var b strings.Builder
		fmt.Fprintf(&b, "current session: %s\n", m.orch.CurrentSessionID())
		if sessions, err := m.orch.SessionManager().List(m.ctx, m.orch.ActiveID(), 10, 0); err == nil {
			for _, s := range sessions {
				fmt.Fprintf(&b, "  %s  %-10s updated %s\n", s.ID, s.Status, s.UpdatedAt.Format("2006-01-02 15:04"))
			}
		}
		m.appendSystem(strings.TrimRight(b.String(), "\n"))
	case "/memory":
		store := m.orch.MemoryStore()
		if store == nil {
			m.appendSystem("memory is disabled")
			break
		}
		records, err := store.List("")
		if err != nil {
			m.appendError(err)
			break
		}
		if len(records) == 0 {
			m.appendSystem("no memory records")
			break
		}
		var b strings.Builder
		for _, rec := range records {
			fmt.Fprintf(&b, "  %s  [%-8s] %s\n", rec.ID, rec.Category, rec.Content)
		}
		m.appendSystem(strings.TrimRight(b.String(), "\n"))
	case "/skills":
		catalog := m.orch.ListSkills()
		if len(catalog) == 0 {
			m.appendSystem("no skills discovered")
			break
		}
		var b strings.Builder
		fmt.Fprintf(&b, "skills (%d):\n", len(catalog))
		for _, skill := range catalog {
			fmt.Fprintf(&b, "  %-24s %s", skill.Name, skill.Description)
			if skill.Source != "" {
				fmt.Fprintf(&b, "  [%s]", skill.Source)
			}
			b.WriteByte('\n')
		}
		m.appendSystem(strings.TrimRight(b.String(), "\n"))
	case "/budget":
		if status := m.orch.BudgetStatusLine(); status != "" {
			m.appendSystem(status)
		}
	case "/workspace":
		if ws := m.orch.Workspace(); ws != nil {
			m.appendSystem(ws.Banner())
		}
	default:
		m.appendError(fmt.Errorf("unknown command: %s (try /help)", cmd))
	}
	m.viewport.SetContent(m.renderTranscript())
	m.viewport.GotoBottom()
	return m, nil
}

// handleModelCommand implements /model. With no argument it shows the
// active agent's current provider/model (with its context window, if
// known), then a model list — fetched live from the active provider's own
// API when that provider supports it and a credential is resolvable
// (Orchestrator.ListActiveProviderModels), clearly labeled as such.
// Otherwise it falls back to modelinfo's static registry, but restricted
// to providers Orchestrator.AuthorizedProviders confirms are actually
// usable right now — never the full catalog regardless of what's
// configured, since a wall of models you can't use is noise, not help. If
// nothing is authorized at all, it says so instead of listing anything.
// Context window size is never available from either vendor's API, so
// that one field always comes from the static table regardless of which
// list is shown. With an argument it switches the active agent's model
// via Orchestrator.SwitchModel, which resolves credentials through the
// full auth precedence chain automatically. A model-only argument uses the
// static registry when possible and otherwise keeps the active provider, which
// allows newly released live-listed models to be selected without repeating it.
func (m *appModel) handleModelCommand(arg string) {
	if arg == "" {
		provider, modelID := m.orch.ActiveModelInfo()
		var b strings.Builder
		fmt.Fprintf(&b, "active: %s / %s", provider, modelID)
		if info, ok := modelinfo.Lookup(provider, modelID); ok {
			fmt.Fprintf(&b, "  (context window: %s tokens)", formatTokenCount(info.ContextWindow))
		}
		b.WriteString("\n\n")

		fetchCtx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		list, live := m.orch.ListActiveProviderModels(fetchCtx)
		cancel()
		switch {
		case live:
			fmt.Fprintf(&b, "models (live from %s API):\n", provider)
		default:
			authorized := m.orch.AuthorizedProviders(m.ctx, distinctProviders(modelinfo.All()))
			list = filterByProviders(modelinfo.All(), authorized)
			if len(list) == 0 {
				b.WriteString("no provider is authorized yet — run /login to add one.")
				m.appendSystem(strings.TrimRight(b.String(), "\n"))
				return
			}
			b.WriteString("models (static registry, authorized providers only):\n")
		}
		for _, i := range list {
			fmt.Fprintf(&b, "  %-11s %-30s %s tokens\n", i.Provider, i.Model, formatTokenCount(i.ContextWindow))
		}
		b.WriteString("\nswitch with: /model <provider> <model>  (or /model <model> if it's unambiguous)")
		m.appendSystem(strings.TrimRight(b.String(), "\n"))
		return
	}

	parts := strings.Fields(arg)
	var provider, modelID string
	switch len(parts) {
	case 1:
		info, ok := modelinfo.LookupByModel(parts[0])
		if ok {
			provider, modelID = info.Provider, parts[0]
		} else {
			provider, _ = m.orch.ActiveModelInfo()
			if provider == "" {
				m.appendError(fmt.Errorf("cannot infer a provider for model %q; use /model <provider> <model>", parts[0]))
				return
			}
			modelID = parts[0]
		}
	case 2:
		provider, modelID = parts[0], parts[1]
	default:
		m.appendError(fmt.Errorf("usage: /model <provider> <model>"))
		return
	}

	if err := m.orch.SwitchModel(m.ctx, provider, modelID); err != nil {
		m.appendError(err)
		return
	}
	m.appendSystem(fmt.Sprintf("switched to %s / %s", provider, modelID))
}

// distinctProviders returns the unique provider names present in list, in
// first-seen order.
func distinctProviders(list []modelinfo.Info) []string {
	seen := make(map[string]bool)
	var out []string
	for _, i := range list {
		if !seen[i.Provider] {
			seen[i.Provider] = true
			out = append(out, i.Provider)
		}
	}
	return out
}

// filterByProviders returns the subset of list whose Provider is in
// providers.
func filterByProviders(list []modelinfo.Info, providers []string) []modelinfo.Info {
	allow := make(map[string]bool, len(providers))
	for _, p := range providers {
		allow[p] = true
	}
	var out []modelinfo.Info
	for _, i := range list {
		if allow[i.Provider] {
			out = append(out, i)
		}
	}
	return out
}

// handleLoginCommand implements /login <provider> <api-key> (the always-
// available BYO-key path), /login openai subscription (the ChatGPT
// browser-login flow — see auth.OpenAICodexSubscriptionConfig for why
// this is OpenAI-only), and /login <provider> oauth <client-id> <auth-url>
// <token-url> (bring-your-own-IdP OAuth). It returns a non-nil tea.Cmd
// only for the two OAuth paths, which run asynchronously so the browser
// round-trip doesn't block the UI.
func (m *appModel) handleLoginCommand(arg string) tea.Cmd {
	parts := strings.Fields(arg)
	if len(parts) < 2 {
		m.appendError(fmt.Errorf("usage: /login <provider> <api-key>  or  /login openai subscription  or  /login <provider> oauth <client-id> <auth-url> <token-url>"))
		return nil
	}
	provider := parts[0]
	if parts[1] == "subscription" {
		if provider != "openai" {
			m.appendError(fmt.Errorf("subscription login is only available for openai — Anthropic disabled third-party subscription OAuth in April 2026; use an API key or reuse an existing Claude Code login instead"))
			return nil
		}
		return m.startSubscriptionLogin()
	}
	if parts[1] == "oauth" {
		if len(parts) < 5 {
			m.appendError(fmt.Errorf("usage: /login <provider> oauth <client-id> <auth-url> <token-url>"))
			return nil
		}
		cfg := auth.ProviderOAuthConfig{
			Provider:     provider,
			ClientID:     parts[2],
			AuthURL:      parts[3],
			TokenURL:     parts[4],
			RedirectPort: 8765,
		}
		m.appendSystem(fmt.Sprintf("starting OAuth login for %q — opening your browser...", provider))
		return m.startOAuthLogin(provider, cfg)
	}

	apiKey := parts[1]
	if err := m.orch.Login(m.ctx, provider, apiKey); err != nil {
		m.appendError(err)
		return nil
	}
	m.appendSystem(fmt.Sprintf("stored API key for %q", provider))
	return nil
}

// startSubscriptionLogin kicks off the OpenAI/ChatGPT subscription browser
// login. See auth.OpenAICodexSubscriptionConfig's doc comment for why this
// exists only for OpenAI and not Anthropic.
func (m *appModel) startSubscriptionLogin() tea.Cmd {
	m.appendSystem("starting ChatGPT subscription login — opening your browser...")
	return m.startOAuthLogin("openai", auth.OpenAICodexSubscriptionConfig())
}

// startOAuthLogin runs Orchestrator.LoginOAuth on a background goroutine,
// relaying its onPromptURL callback and final result back to Update via
// oauthEventMsg — the same self-reissuing channel pattern listenStream
// uses for chat streaming, since a tea.Cmd can only return one message per
// invocation.
func (m *appModel) startOAuthLogin(provider string, cfg auth.ProviderOAuthConfig) tea.Cmd {
	ch := make(chan oauthEvent, 2)
	go func() {
		err := m.orch.LoginOAuth(m.ctx, cfg, func(url string) { ch <- oauthEvent{url: url} })
		ch <- oauthEvent{done: true, err: err}
	}()
	return listenOAuth(ch)
}

func listenOAuth(ch <-chan oauthEvent) tea.Cmd {
	return func() tea.Msg {
		return oauthEventMsg{ev: <-ch, ch: ch}
	}
}

func (m *appModel) handleOAuthEvent(msg oauthEventMsg) (tea.Model, tea.Cmd) {
	if msg.ev.url != "" {
		m.appendSystem("open this URL to sign in:\n  " + msg.ev.url)
		m.viewport.SetContent(m.renderTranscript())
		m.viewport.GotoBottom()
		return m, listenOAuth(msg.ch)
	}
	if msg.ev.err != nil {
		m.appendError(msg.ev.err)
	} else {
		m.appendSystem("OAuth login complete")
	}
	m.viewport.SetContent(m.renderTranscript())
	if m.followOutput {
		m.viewport.GotoBottom()
	}
	return m, nil
}

// handleWhoamiCommand implements /whoami [provider]: with no argument it
// reports the active agent's own provider (the credential that actually
// matters for what you're doing right now), falling back to anthropic and
// openai if no agent/model is active yet.
func (m *appModel) handleWhoamiCommand(arg string) {
	var providers []string
	switch {
	case arg != "":
		providers = []string{arg}
	default:
		if p, _ := m.orch.ActiveModelInfo(); p != "" {
			providers = []string{p}
		} else {
			providers = []string{"anthropic", "openai"}
		}
	}
	var b strings.Builder
	for _, p := range providers {
		fmt.Fprintln(&b, m.orch.AuthStatusLine(m.ctx, p))
	}
	m.appendSystem(strings.TrimRight(b.String(), "\n"))
}

// handleContextCommand implements /context: active model, its known
// context window (if any), the most recent turn's token usage, and the
// session-wide budget line.
func (m *appModel) handleContextCommand() {
	provider, modelID := m.orch.ActiveModelInfo()
	var b strings.Builder
	fmt.Fprintf(&b, "model: %s / %s\n", provider, modelID)
	if info, ok := modelinfo.Lookup(provider, modelID); ok {
		fmt.Fprintf(&b, "context window: %s tokens\n", formatTokenCount(info.ContextWindow))
	} else {
		b.WriteString("context window: unknown (model not in registry)\n")
	}
	if m.lastKnownUsage.PromptTokens > 0 || m.lastKnownUsage.CompletionTokens > 0 {
		fmt.Fprintf(&b, "last turn: %d prompt + %d completion = %d total\n",
			m.lastKnownUsage.PromptTokens, m.lastKnownUsage.CompletionTokens,
			m.lastKnownUsage.PromptTokens+m.lastKnownUsage.CompletionTokens)
	}
	if status := m.orch.BudgetStatusLine(); status != "" {
		fmt.Fprintln(&b, status)
	}
	m.appendSystem(strings.TrimRight(b.String(), "\n"))
}

// formatTokenCount renders a token count compactly (e.g. "12.3k", "1.0M")
// for status bar / picker display; non-positive counts (the modelinfo
// "unknown" sentinel) render as "unknown" rather than "0".
func formatTokenCount(n int) string {
	switch {
	case n <= 0:
		return "unknown"
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// contextUsageSegment renders a compact "ctx used/window (pct%)" status bar
// fragment from the most recent turn's usage, or "" if no turn has
// completed yet. The window comes from modelinfo; an unregistered model
// still shows the used-token count, just without a window/percentage.
func (m *appModel) contextUsageSegment() string {
	used := m.lastKnownUsage.PromptTokens + m.lastKnownUsage.CompletionTokens
	if used == 0 {
		return ""
	}
	provider, modelID := m.orch.ActiveModelInfo()
	info, ok := modelinfo.Lookup(provider, modelID)
	if !ok || info.ContextWindow <= 0 {
		return "ctx " + formatTokenCount(used)
	}
	pct := int(float64(used) / float64(info.ContextWindow) * 100)
	return fmt.Sprintf("ctx %s/%s (%d%%)", formatTokenCount(used), formatTokenCount(info.ContextWindow), pct)
}

// refreshPrompt updates the input box's prompt to show the currently active
// agent (e.g. after /agent, @agent, or auto-routing switches it), matching
// textarea's documented requirement to re-call SetWidth after changing
// Prompt so its internal wrap-width cache stays correct.
func (m *appModel) refreshPrompt() {
	if m.orch.ActiveID() == m.orch.PrimaryID() {
		m.input.Prompt = "❯ "
	} else {
		m.input.Prompt = styleAgentName.Render(m.orch.ActiveID()) + " ❯ "
	}
	if m.width > 0 {
		m.input.SetWidth(m.width - inputBoxBorderWidth - inputBoxPaddingWidth)
	}
}

// appendUserTurn, appendSystem and appendError all wrap to m.viewport.Width():
// the viewport itself never wraps long lines, so an unwrapped line can
// overflow into and visually corrupt the fixed-height chrome below it — the
// same class of bug that made the status bar wrap onto a second line (see
// styleHeaderBar's comment in styles.go).
func (m *appModel) appendUserTurn(line string) {
	header := RenderTurnHeader("❯", "you", styleUserPrefix, m.viewport.Width())
	body := wrapText(line, m.viewport.Width())
	m.blocks = append(m.blocks, header+"\n"+body)
	m.viewport.SetContent(m.renderTranscript())
	m.viewport.GotoBottom()
}

func (m *appModel) appendSystem(s string) {
	m.blocks = append(m.blocks, wrapText(styleDim.Render(s), m.viewport.Width()))
}

func (m *appModel) appendError(err error) {
	m.blocks = append(m.blocks, wrapText(styleError.Render("error: ")+err.Error(), m.viewport.Width()))
}

// finalizeTurn closes out the in-progress agent turn (streamed or not),
// folding activeAgentText/activeToolLines into a permanent transcript block
// and resetting the in-progress state. err, if non-nil, replaces the turn
// with an error block instead. If the user queued a follow-up message with
// Alt+Enter while this turn was still streaming, it is dispatched now via
// the returned tea.Cmd — the same path a typed Enter would use.
func (m *appModel) finalizeTurn(err error) tea.Cmd {
	m.sending = false
	if err != nil {
		m.blocks = append(m.blocks, styleError.Render("error: "+err.Error()))
	} else {
		var b strings.Builder
		b.WriteString(RenderTurnHeader("✦", m.displayAgentName(), styleAgentName, m.viewport.Width()))
		b.WriteString("\n")
		for _, l := range m.activeToolLines {
			b.WriteString(l)
			b.WriteString("\n")
		}
		b.WriteString(RenderMarkdownLite(m.activeAgentText.String(), m.viewport.Width()))
		m.blocks = append(m.blocks, b.String())
		m.lastAssistantText = m.activeAgentText.String()
	}
	if m.lastUsage.PromptTokens > 0 || m.lastUsage.CompletionTokens > 0 {
		m.lastKnownUsage = m.lastUsage
		m.statusMsg = fmt.Sprintf("tokens: %d prompt + %d completion = %d total",
			m.lastUsage.PromptTokens, m.lastUsage.CompletionTokens,
			m.lastUsage.PromptTokens+m.lastUsage.CompletionTokens)
	}
	if status := m.orch.BudgetStatusLine(); status != "" {
		m.statusMsg = status
	}
	m.activeAgentText.Reset()
	m.activeToolLines = nil
	m.lastChunk = ""
	m.lastUsage = model.Usage{}
	m.viewport.SetContent(m.renderTranscript())
	if m.followOutput {
		m.viewport.GotoBottom()
	}

	if m.queuedMessage == "" {
		return nil
	}
	queued := m.queuedMessage
	m.queuedMessage = ""
	_, cmd := m.handleSubmit(queued)
	return cmd
}

func (m *appModel) renderTranscript() string {
	if m.renderWidth != m.viewport.Width() {
		m.renderedBlocks = nil
		m.renderWidth = m.viewport.Width()
	}

	for i := len(m.renderedBlocks); i < len(m.blocks); i++ {
		m.renderedBlocks = append(m.renderedBlocks, m.blocks[i])
	}

	m.transcriptBuf.Reset()
	for i, rb := range m.renderedBlocks {
		if i > 0 {
			m.transcriptBuf.WriteString("\n\n")
		}
		m.transcriptBuf.WriteString(rb)
	}

	if m.sending {
		if m.transcriptBuf.Len() > 0 {
			m.transcriptBuf.WriteString("\n\n")
		}
		for _, tl := range m.activeToolLines {
			m.transcriptBuf.WriteString(tl)
			m.transcriptBuf.WriteByte('\n')
		}
		if txt := m.activeAgentText.String(); txt != "" {
			m.transcriptBuf.WriteString(RenderTurnHeader("✦", m.displayAgentName(), styleAgentName, m.viewport.Width()))
			m.transcriptBuf.WriteByte('\n')
			m.transcriptBuf.WriteString(RenderMarkdownLite(txt, m.viewport.Width()))
		} else {
			m.transcriptBuf.WriteString(styleDim.Render(m.spin.View() + " thinking..."))
		}
	}

	return m.transcriptBuf.String()
}

func (m *appModel) invalidateRenderCache() {
	m.renderedBlocks = nil
}

func (m *appModel) View() tea.View {
	defer m.perf.recordViewEnd()
	if !m.ready {
		return tea.View{AltScreen: true}
	}

	var bottom string
	switch {
	case m.approval != nil:
		bottom = m.renderApprovalModal()
	case m.wizard != nil:
		bottom = m.renderWizardModal()
	case m.picker != nil:
		bottom = m.renderPickerModal()
	case m.searching:
		bottom = m.renderSearchOverlay()
	default:
		input := styleInputBox.Width(m.width - inputBoxBorderWidth).Render(m.input.View())
		if completions := commandCompletions(m.input.Value()); len(completions) > 0 {
			bottom = lipgloss.JoinVertical(lipgloss.Left, m.renderCommandCompletions(completions), input)
		} else {
			bottom = input
		}
	}

	return tea.View{
		Content:   lipgloss.JoinVertical(lipgloss.Left, m.renderHeaderBar(), m.viewport.View(), bottom, m.renderStatusBar()),
		AltScreen: true,
	}
}

func (m *appModel) renderCommandCompletions(completions []string) string {
	var b strings.Builder
	b.WriteString(styleKeyHint.Render(" tab complete "))
	for i, command := range completions {
		if i > 0 {
			b.WriteString("  ")
		}
		if i == m.completionIdx {
			b.WriteString(styleAgentName.Render(command))
		} else {
			b.WriteString(styleDim.Render(command))
		}
	}
	return truncateToWidth(b.String(), m.width)
}

// renderHeaderBar and renderStatusBar build their output by concatenating
// already-fully-sized plain strings and measuring visible width with
// lipgloss.Width, rather than handing a manually-padded string to a style's
// own Width() (which measures its budget net of padding/border — mixing the
// two bookkeeping styles is what originally caused the status bar to
// overflow by its padding width and wrap onto a second line).
func (m *appModel) renderHeaderBar() string {
	left := " ◆ chronos-code "
	if m.orch.ActiveID() != m.orch.PrimaryID() {
		left = " ◆ chronos-code · @" + m.orch.ActiveID() + " "
	}
	right := ""
	if m.workDir != "" {
		dir := m.workDir
		if home, err := os.UserHomeDir(); err == nil {
			if rel, err := filepath.Rel(home, dir); err == nil && !strings.HasPrefix(rel, "..") {
				dir = "~/" + rel
			}
		}
		right = " " + dir + " "
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	return styleHeaderBar.Render(left + strings.Repeat(" ", gap) + right)
}

func (m *appModel) displayAgentName() string {
	if m.orch.ActiveID() == m.orch.PrimaryID() {
		return "chronos-code"
	}
	return m.orch.ActiveID()
}

func (m *appModel) renderApprovalModal() string {
	var b strings.Builder
	b.WriteString(styleHeader.Render("Permission Required"))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "%s %s\n", styleDim.Render("Tool:"), styleBold.Render(m.approval.toolName))
	var details string
	switch m.approval.toolName {
	case "file_write":
		details = RenderFileWriteDiff(m.approval.args)
	case "shell", "shell_auto":
		details = RenderShellPreview(m.approval.args)
	default:
		if args := FormatArgs(m.approval.args); args != "" {
			details = styleDim.Render("Args:") + " " + args
		}
	}
	if details != "" {
		b.WriteString(truncateApprovalDetails(details, m.approvalDetailBudget()))
		b.WriteByte('\n')
	}
	b.WriteString("\n")
	b.WriteString(styleAgentName.Render("y") + styleDim.Render(" once") + "  ")
	b.WriteString(styleError.Render("n") + styleDim.Render(" deny") + "  ")
	b.WriteString(styleUserPrefix.Render("a") + styleDim.Render(" always tool") + "  ")
	b.WriteString(styleUserPrefix.Render("A") + styleDim.Render(" all session"))
	width := m.width - inputBoxBorderWidth
	if width < 1 {
		width = 1
	}
	return styleApprovalModal.Width(width).Render(b.String())
}

func (m *appModel) approvalDetailBudget() int {
	// Header, tool, blank, actions, border, and fixed TUI chrome consume eight rows.
	budget := m.height - 8
	if budget < 1 {
		return 1
	}
	if budget > 20 {
		return 20
	}
	return budget
}

func truncateApprovalDetails(details string, maxLines int) string {
	lines := strings.Split(details, "\n")
	if len(lines) <= maxLines {
		return details
	}
	if maxLines == 1 {
		return styleDim.Render(fmt.Sprintf("... %d detail lines hidden", len(lines)))
	}
	visible := append([]string(nil), lines[:maxLines-1]...)
	visible = append(visible, styleDim.Render(fmt.Sprintf("... %d more lines", len(lines)-maxLines+1)))
	return strings.Join(visible, "\n")
}

func (m *appModel) renderSearchOverlay() string {
	var b strings.Builder
	fmt.Fprintf(&b, "(reverse-i-search)`%s': ", m.searchQuery)
	if len(m.searchResults) > 0 {
		b.WriteString(m.searchResults[m.searchIdx])
	}
	width := m.width - inputBoxBorderWidth
	if width < 1 {
		width = 1
	}
	return styleInputBox.Width(width).Render(b.String())
}

func (m *appModel) renderStatusBar() string {
	streamLabel := "batch"
	if m.stream {
		streamLabel = "stream"
	}
	leftText := " ● " + streamLabel
	if m.orch.ActiveID() != m.orch.PrimaryID() {
		leftText = " ● @" + m.orch.ActiveID() + " │ " + streamLabel
	}
	if ctxSeg := m.contextUsageSegment(); ctxSeg != "" {
		leftText += " │ " + ctxSeg
	}
	leftText += " "
	leftSeg := styleStatusLeft.Render(leftText)

	var rightParts []string
	if m.statusMsg != "" {
		rightParts = append(rightParts, m.statusMsg)
	}
	rightParts = append(rightParts, "ctrl+a agents │ ctrl+/ palette │ ctrl+c quit")
	rightText := " " + strings.Join(rightParts, " │ ") + " "
	rightSeg := styleStatusRight.Render(rightText)

	gap := m.width - lipgloss.Width(leftSeg) - lipgloss.Width(rightSeg)
	if gap < 0 {
		gap = 0
	}
	return leftSeg + styleStatusFill.Render(strings.Repeat(" ", gap)) + rightSeg
}
