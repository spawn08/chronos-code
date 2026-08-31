package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"

	"github.com/spawn08/chronos-code/internal/memory"
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

	blocks          []string // finalized, already-rendered transcript entries
	activeAgentText strings.Builder
	activeToolLines []string
	lastChunk       string
	lastUsage       model.Usage
	sending         bool

	statusMsg string

	approval *pendingApproval

	searching     bool
	searchQuery   string
	searchResults []string
	searchIdx     int

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
		orch:    orch,
		stream:  stream,
		ctx:     ctx,
		cancel:  cancel,
		input:   ta,
		spin:    spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		history: NewHistory(),
		workDir: wd,
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	installApprovalHandlers(orch, NewApprovalHandler(p))

	_, err := p.Run()
	return err
}

func installApprovalHandlers(orch *orchestrator.Orchestrator, handler tool.ApprovalFunc) {
	for _, id := range orch.ListAgents() {
		if a, ok := orch.GetAgent(id); ok {
			a.Tools.SetApprovalHandler(handler)
		}
	}
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
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		vh := msg.Height - headerHeight - (inputRows + inputBoxBorderWidth) - statusHeight
		if vh < 1 {
			vh = 1
		}
		if !m.ready {
			m.viewport = viewport.New(msg.Width, vh)
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = vh
		}
		m.input.SetWidth(msg.Width - inputBoxBorderWidth - inputBoxPaddingWidth)
		m.refreshPrompt()
		m.viewport.SetContent(m.renderTranscript())
		return m, nil

	case tea.KeyMsg:
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
		return m, nil

	case streamStartedMsg:
		return m, listenStream(msg.ch)

	case streamDeltaMsg:
		return m.handleStreamDelta(msg)

	case streamDoneMsg:
		m.finalizeTurn(nil)
		return m, nil

	case chatDoneMsg:
		if msg.resp != nil {
			for _, tc := range msg.resp.ToolCalls {
				m.activeToolLines = append(m.activeToolLines, RenderToolCall(tc.Name, SummarizeArgs(tc.Arguments)))
			}
			m.activeAgentText.WriteString(msg.resp.Content)
			m.lastUsage = msg.resp.Usage
		}
		m.finalizeTurn(msg.err)
		return m, nil

	case shellDoneMsg:
		if msg.err != nil {
			m.appendError(msg.err)
		}
		return m, nil
	}

	return m, nil
}

func (m *appModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, keys.Quit) {
		m.cancel()
		m.quitting = true
		return m, tea.Quit
	}
	if m.approval != nil {
		return m.handleApprovalKey(msg)
	}
	if m.searching {
		return m.handleSearchKey(msg)
	}

	switch {
	case key.Matches(msg, keys.Submit):
		line := strings.TrimSpace(m.input.Value())
		if line == "" {
			return m, nil
		}
		m.input.Reset()
		return m.handleSubmit(line)
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
	return m, cmd
}

func (m *appModel) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	a := m.approval
	switch msg.String() {
	case "y", "enter":
		a.resp <- approvalDecision{allow: true}
	case "a":
		a.resp <- approvalDecision{allow: true, always: true}
	case "n", "esc":
		a.resp <- approvalDecision{allow: false}
	default:
		return m, nil
	}
	m.approval = nil
	return m, nil
}

func (m *appModel) handleSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
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
	case tea.KeyUp, tea.KeyCtrlR:
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
	case tea.KeyRunes, tea.KeySpace:
		m.searchQuery += msg.String()
		m.updateSearchResults()
		return m, nil
	}
	return m, nil
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
	default:
		if agentID, matched := m.orch.Route(m.ctx, line); matched {
			_ = m.orch.SwitchAgent(agentID)
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
		m.finalizeTurn(resp.Err)
		return m, nil
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
	m.viewport.GotoBottom()
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
	case "/stream":
		m.stream = !m.stream
		m.appendSystem(fmt.Sprintf("streaming: %v", m.stream))
	case "/clear":
		m.blocks = nil
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

// refreshPrompt updates the input box's prompt to show the currently active
// agent (e.g. after /agent, @agent, or auto-routing switches it), matching
// textarea's documented requirement to re-call SetWidth after changing
// Prompt so its internal wrap-width cache stays correct.
func (m *appModel) refreshPrompt() {
	m.input.Prompt = styleAgentName.Render(m.orch.ActiveID()) + " ❯ "
	if m.width > 0 {
		m.input.SetWidth(m.width - inputBoxBorderWidth - inputBoxPaddingWidth)
	}
}

// appendUserTurn, appendSystem and appendError all wrap to m.viewport.Width:
// the viewport itself never wraps long lines, so an unwrapped line can
// overflow into and visually corrupt the fixed-height chrome below it — the
// same class of bug that made the status bar wrap onto a second line (see
// styleHeaderBar's comment in styles.go).
func (m *appModel) appendUserTurn(line string) {
	header := RenderTurnHeader("❯", "you", styleUserPrefix, m.viewport.Width)
	body := wrapText(line, m.viewport.Width)
	m.blocks = append(m.blocks, header+"\n"+body)
	m.viewport.SetContent(m.renderTranscript())
	m.viewport.GotoBottom()
}

func (m *appModel) appendSystem(s string) {
	m.blocks = append(m.blocks, wrapText(styleDim.Render(s), m.viewport.Width))
}

func (m *appModel) appendError(err error) {
	m.blocks = append(m.blocks, wrapText(styleError.Render("error: ")+err.Error(), m.viewport.Width))
}

// finalizeTurn closes out the in-progress agent turn (streamed or not),
// folding activeAgentText/activeToolLines into a permanent transcript block
// and resetting the in-progress state. err, if non-nil, replaces the turn
// with an error block instead.
func (m *appModel) finalizeTurn(err error) {
	m.sending = false
	if err != nil {
		m.blocks = append(m.blocks, styleError.Render("error: "+err.Error()))
	} else {
		var b strings.Builder
		b.WriteString(RenderTurnHeader("✦", m.orch.ActiveID(), styleAgentName, m.viewport.Width))
		b.WriteString("\n")
		for _, l := range m.activeToolLines {
			b.WriteString(l)
			b.WriteString("\n")
		}
		b.WriteString(RenderMarkdownLite(m.activeAgentText.String(), m.viewport.Width))
		m.blocks = append(m.blocks, b.String())
	}
	if m.lastUsage.PromptTokens > 0 || m.lastUsage.CompletionTokens > 0 {
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
	m.viewport.GotoBottom()
}

func (m *appModel) renderTranscript() string {
	parts := append([]string{}, m.blocks...)
	if m.sending {
		var cur []string
		cur = append(cur, m.activeToolLines...)
		if txt := m.activeAgentText.String(); txt != "" {
			cur = append(cur, RenderTurnHeader("✦", m.orch.ActiveID(), styleAgentName, m.viewport.Width)+"\n"+RenderMarkdownLite(txt, m.viewport.Width))
		} else {
			cur = append(cur, styleDim.Render(m.spin.View()+" thinking..."))
		}
		parts = append(parts, strings.Join(cur, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

func (m *appModel) View() string {
	if !m.ready {
		return ""
	}

	var bottom string
	switch {
	case m.approval != nil:
		bottom = m.renderApprovalModal()
	case m.searching:
		bottom = m.renderSearchOverlay()
	default:
		bottom = styleInputBox.Width(m.width - inputBoxBorderWidth).Render(m.input.View())
	}

	return lipgloss.JoinVertical(lipgloss.Left, m.renderHeaderBar(), m.viewport.View(), bottom, m.renderStatusBar())
}

// renderHeaderBar and renderStatusBar build their output by concatenating
// already-fully-sized plain strings and measuring visible width with
// lipgloss.Width, rather than handing a manually-padded string to a style's
// own Width() (which measures its budget net of padding/border — mixing the
// two bookkeeping styles is what originally caused the status bar to
// overflow by its padding width and wrap onto a second line).
func (m *appModel) renderHeaderBar() string {
	left := " ◆ chronos-code "
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

func (m *appModel) renderApprovalModal() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", styleHeader.Render("⚠ Permission Required"))
	fmt.Fprintf(&b, "%s  %s\n\n", styleDim.Render("Tool:"), styleBold.Render(m.approval.toolName))
	switch m.approval.toolName {
	case "file_write":
		b.WriteString(RenderFileWriteDiff(m.approval.args))
		b.WriteString("\n")
	case "shell", "shell_auto":
		b.WriteString(RenderShellPreview(m.approval.args))
		b.WriteString("\n")
	default:
		if s := FormatArgs(m.approval.args); s != "" {
			fmt.Fprintf(&b, "%s  %s\n", styleDim.Render("Args:"), s)
		}
	}
	b.WriteString("\n")
	b.WriteString(styleAgentName.Render("y") + styleDim.Render(" allow") + "  ")
	b.WriteString(styleError.Render("n") + styleDim.Render(" deny") + "  ")
	b.WriteString(styleUserPrefix.Render("a") + styleDim.Render(" always"))
	width := m.width - inputBoxBorderWidth
	if width < 1 {
		width = 1
	}
	return styleModal.Width(width).Render(b.String())
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
	leftText := " ● " + m.orch.ActiveID() + " │ " + streamLabel + " "
	leftSeg := styleStatusLeft.Render(leftText)

	var rightParts []string
	if m.statusMsg != "" {
		rightParts = append(rightParts, m.statusMsg)
	}
	rightParts = append(rightParts, "ctrl+c quit")
	rightText := " " + strings.Join(rightParts, " │ ") + " "
	rightSeg := styleStatusRight.Render(rightText)

	gap := m.width - lipgloss.Width(leftSeg) - lipgloss.Width(rightSeg)
	if gap < 0 {
		gap = 0
	}
	return leftSeg + styleStatusFill.Render(strings.Repeat(" ", gap)) + rightSeg
}
