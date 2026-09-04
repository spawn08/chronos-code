package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"

	"github.com/spawn08/chronos/engine/model"
	chronosstream "github.com/spawn08/chronos/engine/stream"
	"github.com/spawn08/chronos/engine/tool"

	"github.com/spawn08/chronos-code/internal/apierror"
	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos-code/internal/budget"
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
	minInputRows         = 1
	maxInputRows         = 8
	inputBoxBorderWidth  = 2 // rounded border, left + right
	inputBoxPaddingWidth = 2 // styleInputBox.Padding(0, 1), left + right
	statusHeight         = 1
	maxTranscriptBytes   = 4 << 20
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
	turnID        uint64
	ctx           context.Context
	ch            <-chan *model.ChatResponse
	contextReport orchestrator.ContextReport
	memoryIntent  *memory.IntentResult
}

type streamDeltaMsg struct {
	turnID uint64
	ctx    context.Context
	resp   *model.ChatResponse
	ch     <-chan *model.ChatResponse
}

type streamDoneMsg struct {
	turnID uint64
	err    error
}

type streamRenderTickMsg struct{}

type activityMsg struct {
	turnID uint64
	ctx    context.Context
	event  chronosstream.Event
	ch     <-chan chronosstream.Event
}

type activityDoneMsg struct{ turnID uint64 }

type turnItemKind uint8

const (
	turnItemText turnItemKind = iota
	turnItemActivity
)

type turnItem struct {
	kind          turnItemKind
	content       string
	rendered      string
	renderedWidth int
}

// chatDoneMsg carries the result of a non-streaming orch.Chat call.
type chatDoneMsg struct {
	turnID        uint64
	resp          *model.ChatResponse
	contextReport orchestrator.ContextReport
	memoryIntent  *memory.IntentResult
	err           error
}

type subagentDoneMsg struct {
	turnID uint64
	name   string
	result string
	err    error
}

type shellDoneMsg struct{ err error }

type clipboardWriteResultMsg struct {
	err      error
	okStatus string
}

type clipboardReadResultMsg struct {
	content string
	err     error
}

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

	viewport       viewport.Model
	input          textarea.Model
	spin           spinner.Model
	history        *History
	clipboardRead  func() (string, error)
	clipboardWrite func(string) error

	width, height int
	workDir       string
	ready         bool

	blocks              []string // finalized, already-rendered transcript entries
	blockBytes          int
	trimmedBlocks       int
	finalizedText       string
	finalizedDirty      bool
	finalizedCount      int
	transcriptBuf       strings.Builder
	activeAgentText     strings.Builder
	activeTurnItems     []turnItem
	activityIndex       map[string]int
	activityArgs        map[string]any
	pendingToolCalls    int
	pendingSubagents    int
	turnModelCalls      int
	turnSubagents       int
	lastModelCalls      int
	lastSubagents       int
	lastChunk           string
	lastAssistantText   string
	lastTurnItems       []turnItem
	lastTurnErr         error
	lastTurnInterrupted bool
	lastTurnBlockIdx    int
	hasLastTurn         bool
	toolsExpanded       bool
	activeRequest       string
	activeSkill         string
	budgetRetried       bool
	lastUsage           model.Usage
	// lastKnownUsage persists the most recent non-zero lastUsage across
	// turns (finalizeTurn zeroes lastUsage itself once each turn's status
	// line is computed), so /context and the status bar's context-usage
	// segment have something to show between turns, not just immediately
	// after one completes.
	lastKnownUsage    model.Usage
	lastContextReport *orchestrator.ContextReport
	lastMemoryIntent  *memory.IntentResult
	turnCostStart     budget.SessionCost
	lastTurnCost      budget.SessionCost
	sending           bool
	turnID            uint64
	turnCtx           context.Context
	turnCancel        context.CancelFunc
	turnInterrupted   bool
	renderScheduled   bool
	activityCh        <-chan chronosstream.Event
	stopActivity      func()

	statusMsg    string
	perf         frameTiming
	followOutput bool
	mouseCapture bool
	bottomView   string
	bottomModal  bool

	approval *pendingApproval
	wizard   *loginWizard
	picker   *picker

	queuedMessages []string

	searching     bool
	searchQuery   string
	searchResults []string
	searchIdx     int
	completionIdx int

	homeDir            string
	completionCacheKey string
	completionCache    []string
	completionCached   bool
	viewportViewCache  string
	viewportViewValid  bool
	headerCache        string
	headerCacheWidth   int
	headerCacheAgent   string
	headerCacheDir     string

	quitting bool
}

// RunTUI replaces the old bufio.Scanner-based REPL (NewREPL/Start) with a
// bubbletea program: scrollback viewport, multi-line input with history,
// markdown-lite response rendering, and a modal-based permission prompt that
// doesn't fight bubbletea for stdin the way a second bufio.Reader would.
func RunTUI(orch *orchestrator.Orchestrator, stream bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ta := newComposer()
	ta.Focus()

	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()

	m := &appModel{
		orch:           orch,
		stream:         stream,
		ctx:            ctx,
		cancel:         cancel,
		input:          ta,
		spin:           spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		history:        NewHistory(),
		clipboardRead:  clipboard.ReadAll,
		clipboardWrite: clipboard.WriteAll,
		mouseCapture:   false,
		workDir:        wd,
		homeDir:        home,
		followOutput:   true,
		statusMsg:      orch.StartupHints(ctx),
	}

	p := tea.NewProgram(m)
	installApprovalHandlers(orch, NewApprovalHandler(p))

	_, err := p.Run()
	return err
}

func newComposer() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "Message chronos-code..."
	ta.Prompt = "❯ "
	ta.ShowLineNumbers = false
	ta.DynamicHeight = true
	ta.MinHeight = minInputRows
	ta.MaxHeight = maxInputRows
	ta.MaxContentHeight = 500
	ta.SetHeight(minInputRows)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	return ta
}

type approvalHandlerInstaller interface {
	SetApprovalHandler(tool.ApprovalFunc)
}

func installApprovalHandlers(installer approvalHandlerInstaller, handler tool.ApprovalFunc) {
	installer.SetApprovalHandler(handler)
}

func (m *appModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.input.Focus())
}

func (m *appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.perf.recordUpdateStart()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		contentWidth := msg.Width
		if contentWidth < 1 {
			contentWidth = 1
		}
		if !m.ready {
			m.viewport = viewport.New()
			m.viewport.SetWidth(contentWidth)
			m.ready = true
		} else {
			m.viewport.SetWidth(contentWidth)
		}
		m.refreshPrompt()
		m.resizeViewport()
		m.setViewportContent(m.renderTranscript())
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.MouseWheelMsg:
		if !m.ready || !m.mouseCapture {
			return m, nil
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		m.followOutput = m.viewport.AtBottom()
		m.viewportViewValid = false
		return m, cmd

	case tea.PasteMsg:
		if m.approval != nil || m.wizard != nil || m.picker != nil || m.searching {
			return m, nil
		}
		m.input.InsertString(msg.Content)
		m.resizeViewport()
		return m, nil

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
		if msg.turnID != m.turnID {
			return m, nil
		}
		m.captureExecutionMetadata(msg.contextReport, msg.memoryIntent)
		return m, listenStream(msg.ctx, msg.turnID, msg.ch)

	case streamDeltaMsg:
		return m.handleStreamDelta(msg)

	case streamRenderTickMsg:
		m.renderScheduled = false
		m.refreshViewport()
		return m, nil

	case activityMsg:
		return m.handleActivity(msg)

	case activityDoneMsg:
		return m, nil

	case streamDoneMsg:
		if msg.turnID != m.turnID {
			return m, nil
		}
		return m, m.finalizeTurn(msg.err)

	case chatDoneMsg:
		if msg.turnID != m.turnID {
			return m, nil
		}
		m.captureExecutionMetadata(msg.contextReport, msg.memoryIntent)
		if msg.resp != nil {
			if m.activityCh == nil {
				for _, tc := range msg.resp.ToolCalls {
					m.appendTurnActivity(RenderToolCall(tc.Name, SummarizeArgs(tc.Arguments)))
				}
			}
			m.appendTurnText(msg.resp.Content)
			m.lastUsage = msg.resp.Usage
		}
		return m, m.finalizeTurn(msg.err)

	case subagentDoneMsg:
		if msg.turnID != m.turnID {
			return m, nil
		}
		if idx, ok := m.activityIndex["direct-subagent"]; ok {
			m.activeTurnItems[idx].content = RenderToolActivity("", "subagent:"+msg.name, m.activityArgs["direct-subagent"], true, msg.err)
		}
		m.pendingToolCalls = 0
		m.pendingSubagents = 0
		if msg.err == nil {
			m.appendTurnText(msg.result)
		}
		return m, m.finalizeTurn(msg.err)

	case shellDoneMsg:
		if msg.err != nil {
			m.appendError(msg.err)
		}
		return m, nil

	case clipboardWriteResultMsg:
		if msg.err != nil {
			m.statusMsg = "copy failed: " + msg.err.Error()
			return m, nil
		}
		if msg.okStatus != "" {
			m.statusMsg = msg.okStatus
		} else {
			m.statusMsg = "copied response"
		}
		return m, nil

	case clipboardReadResultMsg:
		if msg.err != nil {
			m.statusMsg = "paste failed: " + msg.err.Error()
			return m, nil
		}
		m.input.InsertString(msg.content)
		m.statusMsg = "pasted clipboard"
		m.resizeViewport()
		return m, nil

	case oauthEventMsg:
		return m.handleOAuthEvent(msg)
	}

	return m, nil
}

func (m *appModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, keys.Quit) {
		if m.sending {
			m.interruptTurn()
			return m, nil
		}
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
		m.viewportViewValid = false
		return m, cmd
	}
	if m.ready && msg.Mod.Contains(tea.ModCtrl) && (msg.Code == tea.KeyUp || msg.Code == tea.KeyDown) {
		if msg.Code == tea.KeyUp {
			m.viewport.HalfPageUp()
		} else {
			m.viewport.HalfPageDown()
		}
		m.followOutput = m.viewport.AtBottom()
		m.viewportViewValid = false
		return m, nil
	}
	if m.ready && msg.Mod.Contains(tea.ModCtrl) && (msg.Code == tea.KeyHome || msg.Code == tea.KeyEnd) {
		if msg.Code == tea.KeyHome {
			m.viewport.GotoTop()
			m.followOutput = false
		} else {
			m.viewport.GotoBottom()
			m.followOutput = true
		}
		m.viewportViewValid = false
		return m, nil
	}
	if key.Matches(msg, keys.CopyLast) {
		content, okStatus, err := m.copyText("")
		if err != nil {
			m.statusMsg = err.Error()
			return m, nil
		}
		return m, m.copyClipboardCmd(content, okStatus)
	}
	if key.Matches(msg, keys.CopyCode) {
		content, okStatus, err := m.copyText("code")
		if err != nil {
			m.statusMsg = err.Error()
			return m, nil
		}
		return m, m.copyClipboardCmd(content, okStatus)
	}
	if key.Matches(msg, keys.ToggleTools) {
		return m.toggleToolDetails()
	}
	if key.Matches(msg, keys.Paste) {
		m.statusMsg = "pasting"
		read := m.clipboardRead
		return m, func() tea.Msg {
			if read == nil {
				return clipboardReadResultMsg{err: fmt.Errorf("clipboard reader unavailable")}
			}
			content, err := read()
			return clipboardReadResultMsg{content: content, err: err}
		}
	}
	if completions := m.inputCompletions(); len(completions) > 0 {
		if m.completionIdx >= len(completions) {
			m.completionIdx = 0
		}
		switch msg.Code {
		case tea.KeyTab:
			m.input.SetValue(applyCompletion(m.input.Value(), completions[m.completionIdx]))
			m.input.CursorEnd()
			m.completionIdx = 0
			m.resizeViewport()
			return m, nil
		case tea.KeyUp:
			m.completionIdx = (m.completionIdx - 1 + len(completions)) % len(completions)
			m.resizeViewport()
			return m, nil
		case tea.KeyDown:
			m.completionIdx = (m.completionIdx + 1) % len(completions)
			m.resizeViewport()
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
		if m.sending {
			m.queuedMessages = append([]string{line}, m.queuedMessages...)
			m.interruptTurn()
			return m, nil
		}
		return m.handleSubmit(line)
	case msg.String() == "alt+enter" && m.sending:
		line := strings.TrimSpace(m.input.Value())
		if line == "" {
			return m, nil
		}
		m.queuedMessages = append(m.queuedMessages, line)
		m.input.Reset()
		m.statusMsg = fmt.Sprintf("running │ %d queued", len(m.queuedMessages))
		m.resizeViewport()
		return m, nil
	case key.Matches(msg, keys.AgentPicker):
		m.picker = newAgentPicker(m)
		m.resizeViewport()
		return m, nil
	case key.Matches(msg, keys.ModelPicker):
		m.picker = newModelPicker(m)
		m.resizeViewport()
		return m, nil
	case key.Matches(msg, keys.LoginWizard):
		m.wizard = newLoginWizard(m)
		m.resizeViewport()
		return m, nil
	case key.Matches(msg, keys.CommandPalette):
		m.picker = newCommandPalette()
		m.resizeViewport()
		return m, nil
	case key.Matches(msg, keys.HistoryPrev):
		if !strings.Contains(m.input.Value(), "\n") {
			if v, ok := m.history.Prev(m.input.Value()); ok {
				m.input.SetValue(v)
				m.input.CursorEnd()
				m.resizeViewport()
				return m, nil
			}
		}
	case key.Matches(msg, keys.HistoryNext):
		if !strings.Contains(m.input.Value(), "\n") {
			if v, ok := m.history.Next(); ok {
				m.input.SetValue(v)
				m.input.CursorEnd()
				m.resizeViewport()
				return m, nil
			}
		}
	case key.Matches(msg, keys.ReverseSearch):
		m.searching = true
		m.searchQuery = ""
		m.updateSearchResults()
		m.resizeViewport()
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if completions := m.inputCompletions(); m.completionIdx >= len(completions) {
		m.completionIdx = 0
	}
	m.resizeViewport()
	return m, cmd
}

func (m *appModel) interruptTurn() {
	if !m.sending || m.turnCancel == nil {
		return
	}
	m.turnInterrupted = true
	m.statusMsg = "interrupting..."
	m.turnCancel()
	if m.approval != nil {
		m.approval = nil
		m.resizeViewport()
	}
}

func (m *appModel) viewportHeight() int {
	bottomHeight := lipgloss.Height(m.bottomView)
	height := m.height - headerHeight - bottomHeight - statusHeight
	if m.bottomModal && height < 0 {
		return 0
	}
	if !m.bottomModal && height < 1 {
		return 1
	}
	return height
}

func (m *appModel) resizeViewport() {
	if m.ready {
		maxHeight := maxInputRows
		if available := (m.height - headerHeight - statusHeight - inputBoxBorderWidth) / 3; available < maxHeight {
			maxHeight = available
		}
		if maxHeight < minInputRows {
			maxHeight = minInputRows
		}
		m.input.MaxHeight = maxHeight
		width := m.width - inputBoxBorderWidth - inputBoxPaddingWidth
		if width < 1 {
			width = 1
		}
		if m.input.Width() != width {
			m.input.SetWidth(width)
		}
		m.bottomView, m.bottomModal = m.renderBottom()
		height := m.viewportHeight()
		if m.viewport.Height() != height {
			m.viewport.SetHeight(height)
			m.viewportViewValid = false
		}
	}
}

func (m *appModel) renderBottom() (string, bool) {
	switch {
	case m.approval != nil:
		return m.renderApprovalModal(), true
	case m.wizard != nil:
		return m.renderWizardModal(), true
	case m.picker != nil:
		return m.renderPickerModal(), true
	case m.searching:
		return m.renderSearchOverlay(), true
	default:
		input := styleInputBox.Width(m.width - inputBoxBorderWidth).Render(m.input.View())
		if completions := m.inputCompletions(); len(completions) > 0 {
			return lipgloss.JoinVertical(lipgloss.Left, m.renderCommandCompletions(completions), input), false
		}
		return input, false
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
	defer m.resizeViewport()
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
			m.searchQuery = removeLastRune(m.searchQuery)
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
	displayLine := line
	explicitSkill := ""
	switch {
	case strings.HasPrefix(line, "/"):
		name, task, ok := m.parseSkillInvocation(line)
		if !ok {
			if strings.Fields(line)[0] == "/subagent" {
				return m.handleSubagentCommand(line)
			}
			return m.handleSlashCommand(line)
		}
		if task == "" {
			m.appendError(fmt.Errorf("usage: /%s <task>", name))
			m.refreshViewport()
			return m, nil
		}
		explicitSkill = name
		line = task
	case strings.HasPrefix(line, "!"):
		return m.handleShellEscape(line[1:])
	case strings.HasPrefix(line, "@"):
		parts := strings.SplitN(line[1:], " ", 2)
		if len(parts) == 2 && knownAgent(parts[0], m.orch.ListAgents()) {
			if err := m.orch.SwitchAgent(parts[0]); err != nil {
				m.appendError(err)
				return m, nil
			}
			line = parts[1]
		}
	}
	if root := m.workspaceRoot(); root != "" {
		line = attachReferencedFiles(root, line, m.orch.ListAgents())
	}

	m.history.Add(displayLine)
	m.appendUserTurn(displayLine)
	m.refreshPrompt()

	m.sending = true
	m.activeRequest = line
	m.activeSkill = explicitSkill
	m.budgetRetried = false
	m.turnID++
	m.turnCtx, m.turnCancel = context.WithCancel(m.ctx)
	if explicitSkill != "" {
		var err error
		m.turnCtx, err = m.orch.WithSkill(m.turnCtx, explicitSkill)
		if err != nil {
			m.turnCancel()
			m.sending = false
			m.appendError(err)
			return m, nil
		}
	}
	m.turnInterrupted = false
	turnID := m.turnID
	turnCtx := m.turnCtx
	m.turnCostStart = m.orch.SessionCost()
	m.activeAgentText.Reset()
	m.activeTurnItems = nil
	m.activityIndex = make(map[string]int)
	m.activityArgs = make(map[string]any)
	m.turnModelCalls = 0
	m.turnSubagents = 0
	m.lastChunk = ""
	var activityCmd tea.Cmd
	if ch, stop, err := m.orch.SubscribeActivity(); err == nil {
		m.activityCh = ch
		m.stopActivity = stop
		activityCmd = listenActivity(turnCtx, turnID, ch)
	}
	m.refreshViewport()
	return m, tea.Batch(m.sendCmd(turnCtx, turnID, line), m.spin.Tick, activityCmd)
}

func (m *appModel) parseSkillInvocation(line string) (name, task string, ok bool) {
	parts := strings.SplitN(line, " ", 2)
	commands := append([]string(nil), paletteCommands...)
	commands = append(commands, "/exit", "/q", "/h")
	for _, command := range commands {
		if strings.EqualFold(parts[0], command) {
			return "", "", false
		}
	}
	name = strings.TrimPrefix(parts[0], "/")
	for _, skill := range m.orch.ListSkills() {
		if strings.EqualFold(skill.Name, name) {
			if len(parts) == 2 {
				task = strings.TrimSpace(parts[1])
			}
			return skill.Name, task, true
		}
	}
	return "", "", false
}

func (m *appModel) handleSubagentCommand(line string) (tea.Model, tea.Cmd) {
	arg := strings.TrimSpace(strings.TrimPrefix(line, "/subagent"))
	if arg == "" {
		m.appendError(fmt.Errorf("usage: /subagent <name> <task> or /subagent {JSON}"))
		m.refreshViewport()
		return m, nil
	}

	args := make(map[string]any)
	name := "dynamic"
	if strings.HasPrefix(arg, "{") {
		if err := json.Unmarshal([]byte(arg), &args); err != nil {
			m.appendError(fmt.Errorf("parse /subagent JSON: %w", err))
			m.refreshViewport()
			return m, nil
		}
		if configured, _ := args["agent"].(string); configured != "" {
			name = configured
		}
	} else {
		parts := strings.SplitN(arg, " ", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
			m.appendError(fmt.Errorf("usage: /subagent <name> <task>"))
			m.refreshViewport()
			return m, nil
		}
		name = parts[0]
		args["agent"] = name
		args["task"] = strings.TrimSpace(parts[1])
	}
	if task, _ := args["task"].(string); strings.TrimSpace(task) == "" {
		m.appendError(fmt.Errorf("/subagent requires a non-empty task"))
		m.refreshViewport()
		return m, nil
	}

	m.history.Add(line)
	m.appendUserTurn(line)
	m.sending = true
	m.activeRequest = ""
	m.activeSkill = ""
	m.budgetRetried = false
	m.turnID++
	m.turnCtx, m.turnCancel = context.WithCancel(m.ctx)
	m.turnInterrupted = false
	m.turnCostStart = m.orch.SessionCost()
	m.activeAgentText.Reset()
	m.activeTurnItems = nil
	m.activityIndex = map[string]int{"direct-subagent": 0}
	m.activityArgs = map[string]any{"direct-subagent": args}
	m.pendingToolCalls = 1
	m.pendingSubagents = 1
	m.turnSubagents = 1
	m.appendTurnActivity(RenderToolActivity("", "subagent:"+name, args, false, nil))
	turnID := m.turnID
	turnCtx := m.turnCtx
	var activityCmd tea.Cmd
	if ch, stop, err := m.orch.SubscribeActivity(); err == nil {
		m.activityCh = ch
		m.stopActivity = stop
		activityCmd = listenActivity(turnCtx, turnID, ch)
	}
	m.refreshViewport()
	return m, tea.Batch(func() tea.Msg {
		result, err := m.orch.RunSubagent(turnCtx, args)
		return subagentDoneMsg{turnID: turnID, name: name, result: result, err: err}
	}, m.spin.Tick, activityCmd)
}

func (m *appModel) sendCmd(ctx context.Context, turnID uint64, message string) tea.Cmd {
	orch := m.orch
	stream := m.stream
	return func() tea.Msg {
		if stream {
			result, err := StartExecution(ctx, orch, orchestrator.ExecutionRequest{
				Message:          message,
				Mode:             orchestrator.ExecutionStreaming,
				SessionID:        orch.CurrentSessionID(),
				VerificationMode: orch.VerificationMode(),
			})
			if err != nil {
				return chatDoneMsg{turnID: turnID, contextReport: result.ContextReport, memoryIntent: result.MemoryIntent, err: err}
			}
			return streamStartedMsg{turnID: turnID, ctx: ctx, ch: result.Stream, contextReport: result.ContextReport, memoryIntent: result.MemoryIntent}
		}
		result, err := StartExecution(ctx, orch, orchestrator.ExecutionRequest{
			Message: message, SessionID: orch.CurrentSessionID(), VerificationMode: orch.VerificationMode(),
		})
		return chatDoneMsg{turnID: turnID, resp: result.Response, contextReport: result.ContextReport, memoryIntent: result.MemoryIntent, err: err}
	}
}

func (m *appModel) captureExecutionMetadata(report orchestrator.ContextReport, intent *memory.IntentResult) {
	if len(report.Sources) == 0 {
		return
	}
	report.Sources = append([]orchestrator.ContextSourceReport(nil), report.Sources...)
	m.lastContextReport = &report
	if intent == nil {
		m.lastMemoryIntent = nil
	} else {
		copied := *intent
		m.lastMemoryIntent = &copied
	}
	line := RenderContextSummary(report, intent)
	if idx, ok := m.activityIndex["context-report"]; ok && idx < len(m.activeTurnItems) {
		m.activeTurnItems[idx].content = line
		return
	}
	m.appendTurnActivity(line)
	if m.activityIndex == nil {
		m.activityIndex = make(map[string]int)
	}
	m.activityIndex["context-report"] = len(m.activeTurnItems) - 1
}

// StartExecution starts one TUI turn through the common execution boundary.
func StartExecution(ctx context.Context, orch *orchestrator.Orchestrator, request orchestrator.ExecutionRequest) (orchestrator.ExecutionResult, error) {
	return orch.Execute(ctx, request)
}

func listenStream(ctx context.Context, turnID uint64, ch <-chan *model.ChatResponse) tea.Cmd {
	return func() tea.Msg {
		select {
		case resp, ok := <-ch:
			if !ok {
				return streamDoneMsg{turnID: turnID}
			}
			return streamDeltaMsg{turnID: turnID, ctx: ctx, resp: resp, ch: ch}
		case <-ctx.Done():
			return streamDoneMsg{turnID: turnID, err: ctx.Err()}
		}
	}
}

func listenActivity(ctx context.Context, turnID uint64, ch <-chan chronosstream.Event) tea.Cmd {
	return func() tea.Msg {
		for {
			select {
			case event, ok := <-ch:
				if !ok {
					return activityDoneMsg{turnID: turnID}
				}
				switch event.Type {
				case chronosstream.EventModelCall, chronosstream.EventToolCall, chronosstream.EventToolResult:
					return activityMsg{turnID: turnID, ctx: ctx, event: event, ch: ch}
				}
			case <-ctx.Done():
				return activityDoneMsg{turnID: turnID}
			}
		}
	}
}

func (m *appModel) handleStreamDelta(msg streamDeltaMsg) (tea.Model, tea.Cmd) {
	if msg.turnID != m.turnID {
		return m, nil
	}
	resp := msg.resp
	if resp.Err != nil {
		classified := apierror.Classify(resp.Err)
		if apierror.IsCompactable(classified) && !m.budgetRetried && m.activeRequest != "" {
			m.appendTurnActivity(styleDim.Render("  ↻ " + classified.Message))
			if compactErr := m.orch.CompactActiveSession(m.ctx); compactErr == nil {
				m.budgetRetried = true
				m.refreshViewport()
				return m, m.sendCmd(m.turnCtx, m.turnID, m.activeRequest)
			}
		}
		return m, m.finalizeTurn(resp.Err)
	}
	if resp.Usage.PromptTokens > 0 || resp.Usage.CacheReadTokens > 0 || resp.Usage.CacheCreationTokens > 0 || resp.Usage.CompletionTokens > 0 {
		m.lastUsage.Merge(resp.Usage)
	}
	for _, tc := range resp.ToolCalls {
		if tc.Name == "spawn_subagent" {
			if m.activityIndex == nil {
				m.activityIndex = make(map[string]int)
				m.activityArgs = make(map[string]any)
			}
			key := "stream/" + tc.ID
			if _, exists := m.activityIndex[key]; !exists {
				var args map[string]any
				_ = json.Unmarshal([]byte(tc.Arguments), &args)
				m.appendTurnActivity(RenderToolActivity("", tc.Name, args, false, nil))
				m.activityIndex[key] = len(m.activeTurnItems) - 1
				m.activityArgs[key] = args
				m.pendingToolCalls++
				m.pendingSubagents++
				m.turnSubagents++
			}
		} else if m.activityCh == nil {
			m.appendTurnActivity(RenderToolCall(tc.Name, SummarizeArgs(tc.Arguments)))
			m.pendingToolCalls++
		}
	}
	if text := m.streamText(resp); text != "" {
		if m.activityCh == nil && m.pendingToolCalls > 0 && len(resp.ToolCalls) == 0 {
			label := progressLabel(m.pendingToolCalls, m.pendingSubagents, "completed")
			m.appendTurnActivity(styleAgentName.Render("  ✓ " + label))
			m.pendingToolCalls = 0
			m.pendingSubagents = 0
		}
		m.appendTurnText(text)
	}
	if resp.Reasoning != "" {
		m.appendThinking(resp.Reasoning)
	}
	cmds := []tea.Cmd{listenStream(msg.ctx, msg.turnID, msg.ch)}
	if !m.renderScheduled {
		m.renderScheduled = true
		cmds = append(cmds, tea.Tick(time.Second/30, func(time.Time) tea.Msg { return streamRenderTickMsg{} }))
	}
	return m, tea.Batch(cmds...)
}

func (m *appModel) streamText(resp *model.ChatResponse) string {
	if resp == nil || resp.Content == "" {
		return ""
	}
	if resp.Delta {
		return resp.Content
	}
	current := m.activeAgentText.String()
	if current == "" {
		return resp.Content
	}
	if strings.HasPrefix(resp.Content, current) {
		return strings.TrimPrefix(resp.Content, current)
	}
	if resp.Content == m.lastChunk {
		return ""
	}
	m.lastChunk = resp.Content
	return resp.Content
}

func (m *appModel) handleActivity(msg activityMsg) (tea.Model, tea.Cmd) {
	if msg.turnID != m.turnID || !m.sending {
		return m, nil
	}
	data, _ := msg.event.Data.(map[string]any)
	if m.activityIndex == nil {
		m.activityIndex = make(map[string]int)
		m.activityArgs = make(map[string]any)
	}
	agentID, _ := data["agent"].(string)
	callID, _ := data["id"].(string)
	toolName, _ := data["tool"].(string)
	activityKey := agentID + "/" + callID
	if callID == "" {
		activityKey = agentID + "/" + toolName
	}
	label := ""
	if agentID != "" {
		label = "@" + agentID + " "
	}
	changed := true
	switch msg.event.Type {
	case chronosstream.EventModelCall:
		m.turnModelCalls++
		modelName, _ := data["model"].(string)
		key := "model/" + agentID
		line := RenderModelActivityCount(label, modelName, m.turnModelCalls)
		if idx, ok := m.activityIndex[key]; ok && idx < len(m.activeTurnItems) {
			m.activeTurnItems[idx].content = line
		} else {
			m.appendTurnActivity(line)
			m.activityIndex[key] = len(m.activeTurnItems) - 1
		}
	case chronosstream.EventToolCall:
		line := RenderToolActivity(label, toolName, data["args"], false, data["error"])
		provisionalKey := "stream/" + callID
		if idx, ok := m.activityIndex[provisionalKey]; toolName == "spawn_subagent" && callID != "" && ok {
			m.activeTurnItems[idx].content = line
			m.activityIndex[activityKey] = idx
			delete(m.activityIndex, provisionalKey)
		} else {
			m.appendTurnActivity(line)
			m.activityIndex[activityKey] = len(m.activeTurnItems) - 1
			m.pendingToolCalls++
			if toolName == "spawn_subagent" {
				m.pendingSubagents++
				m.turnSubagents++
			}
		}
		m.activityArgs[activityKey] = data["args"]
	case chronosstream.EventToolResult:
		line := RenderToolActivity(label, toolName, m.activityArgs[activityKey], true, data["error"])
		if idx, ok := m.activityIndex[activityKey]; ok && idx < len(m.activeTurnItems) {
			m.activeTurnItems[idx].content = line
		} else {
			m.appendTurnActivity(line)
		}
		if m.pendingToolCalls > 0 {
			m.pendingToolCalls--
		}
		if toolName == "spawn_subagent" && m.pendingSubagents > 0 {
			m.pendingSubagents--
		}
	case chronosstream.EventCustom:
		eventType, _ := data["type"].(string)
		if eventType == "api_retry" {
			message, _ := data["message"].(string)
			key := "api_retry/" + agentID
			line := styleDim.Render("  ↻ " + message)
			if idx, ok := m.activityIndex[key]; ok && idx < len(m.activeTurnItems) {
				m.activeTurnItems[idx].content = line
			} else {
				m.appendTurnActivity(line)
				m.activityIndex[key] = len(m.activeTurnItems) - 1
			}
		} else {
			changed = false
		}
	default:
		changed = false
	}
	cmds := []tea.Cmd{listenActivity(msg.ctx, msg.turnID, msg.ch)}
	if changed && !m.renderScheduled {
		m.renderScheduled = true
		cmds = append(cmds, tea.Tick(time.Second/30, func(time.Time) tea.Msg { return streamRenderTickMsg{} }))
	}
	return m, tea.Batch(cmds...)
}

func (m *appModel) appendTurnText(text string) {
	if text == "" {
		return
	}
	m.activeAgentText.WriteString(text)
	if n := len(m.activeTurnItems); n > 0 && m.activeTurnItems[n-1].kind == turnItemText {
		item := &m.activeTurnItems[n-1]
		item.content += text
		if item.rendered != "" {
			item.rendered = appendWrappedText(item.rendered, text, item.renderedWidth)
		}
		return
	}
	m.activeTurnItems = append(m.activeTurnItems, turnItem{kind: turnItemText, content: text})
}

func (m *appModel) appendThinking(text string) {
	if text == "" {
		return
	}
	if m.activityIndex == nil {
		m.activityIndex = make(map[string]int)
	}
	const key = "thinking"
	if idx, ok := m.activityIndex[key]; ok && idx < len(m.activeTurnItems) {
		item := &m.activeTurnItems[idx]
		item.content += text
		item.rendered = ""
		return
	}
	m.appendTurnActivity(styleDim.Render("thinking: " + text))
	m.activityIndex[key] = len(m.activeTurnItems) - 1
}

func appendWrappedText(existing, suffix string, width int) string {
	if suffix == "" {
		return existing
	}
	if width <= 0 {
		return existing + suffix
	}
	lastNL := strings.LastIndexByte(existing, '\n')
	prefix := ""
	lastLine := existing
	if lastNL >= 0 {
		prefix = existing[:lastNL+1]
		lastLine = existing[lastNL+1:]
	}
	return prefix + wrapText(lastLine+suffix, width)
}

func (m *appModel) appendTurnActivity(line string) {
	m.activeTurnItems = append(m.activeTurnItems, turnItem{kind: turnItemActivity, content: line})
}

func (m *appModel) setViewportContent(s string) {
	m.viewport.SetContent(s)
	m.viewportViewValid = false
}

func (m *appModel) refreshViewport() {
	m.setViewportContent(m.renderTranscript())
	if m.followOutput {
		m.viewport.GotoBottom()
		m.viewportViewValid = false
	}
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

func (m *appModel) workspaceRoot() string {
	if m.orch != nil {
		if ws := m.orch.Workspace(); ws != nil && ws.Root != "" {
			return ws.Root
		}
	}
	return m.workDir
}

func (m *appModel) handleMCPCommand(arg string) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		m.appendSystem(m.mcpStatusText())
		return
	}
	switch fields[0] {
	case "connect":
		if len(fields) != 2 {
			m.appendError(fmt.Errorf("usage: /mcp connect <name>"))
			return
		}
		status, err := m.orch.ConnectMCP(m.ctx, fields[1])
		if err != nil {
			m.appendError(err)
			return
		}
		m.appendSystem(fmt.Sprintf("connected %s (%d tools)", status.Name, status.Tools))
	default:
		m.appendError(fmt.Errorf("unknown mcp command %q (try /mcp or /mcp connect <name>)", fields[0]))
	}
}

func (m *appModel) mcpStatusText() string {
	statuses := m.orch.MCPStatuses()
	if len(statuses) == 0 {
		return "no MCP servers discovered\nadd one with: chronos-code mcp add <name> --command <cmd>\nor place .mcp.json / .cursor/mcp.json in the project, then /mcp connect <name>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "mcp servers (%d):\n", len(statuses))
	for _, status := range statuses {
		fmt.Fprintf(&b, "  %-24s %s", status.Name, status.State)
		if status.Tools > 0 {
			fmt.Fprintf(&b, "  tools=%d", status.Tools)
		}
		if status.State == "approval_required" {
			fmt.Fprintf(&b, "  (/mcp connect %s)", status.Name)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
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
			role := ""
			if id == m.orch.PrimaryID() {
				role = " (primary)"
			}
			a, ok := m.orch.GetAgent(id)
			name := id
			if ok {
				name = a.Name
			}
			fmt.Fprintf(&b, "%s%s%s — %s\n", marker, id, role, name)
		}
		m.appendSystem(strings.TrimRight(b.String(), "\n"))
	case "/agent":
		if arg == "" {
			active := m.orch.ActiveID()
			if active == m.orch.PrimaryID() {
				m.appendSystem(fmt.Sprintf("active: %s (primary Chronos Code)", active))
			} else {
				m.appendSystem(fmt.Sprintf("active: %s (specialist; primary is %s)", active, m.orch.PrimaryID()))
			}
		} else if err := m.orch.SwitchAgent(arg); err != nil {
			m.appendError(err)
		} else {
			m.appendSystem(fmt.Sprintf("switched to %s", arg))
			m.refreshPrompt()
		}
	case "/model":
		m.handleModelCommand(arg)
	case "/think":
		m.handleThinkCommand(arg)
	case "/login":
		if arg == "" {
			m.wizard = newLoginWizard(m)
			m.resizeViewport()
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
	case "/usage":
		m.appendSystem(m.usageSummary())
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
		m.blockBytes = 0
		m.trimmedBlocks = 0
		m.invalidateRenderCache()
		m.lastKnownUsage = model.Usage{}
		m.lastContextReport = nil
		m.lastMemoryIntent = nil
		m.lastTurnCost = budget.SessionCost{}
		m.lastModelCalls = 0
		m.lastSubagents = 0
		m.lastAssistantText = ""
		m.lastTurnItems = nil
		m.lastTurnErr = nil
		m.lastTurnInterrupted = false
		m.hasLastTurn = false
		m.lastTurnBlockIdx = -1
		m.queuedMessages = nil
		m.statusMsg = "new session started"
		m.followOutput = true
	case "/copy":
		content, okStatus, err := m.copyText(arg)
		if err != nil {
			m.statusMsg = err.Error()
			break
		}
		copyCmd := m.copyClipboardCmd(content, okStatus)
		m.setViewportContent(m.renderTranscript())
		m.viewport.GotoBottom()
		return m, copyCmd
	case "/perf":
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		m.appendSystem(fmt.Sprintf("%s\nmemory: heap=%s allocated=%s sys=%s transcript=%s",
			m.perf.stats(), formatBytes(stats.HeapAlloc), formatBytes(stats.TotalAlloc),
			formatBytes(stats.Sys), formatBytes(uint64(m.transcriptBytes()))))
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
	case "/mcp":
		m.handleMCPCommand(arg)
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
		var b strings.Builder
		if ws := m.orch.Workspace(); ws != nil {
			b.WriteString(ws.Banner())
			b.WriteByte('\n')
		}
		b.WriteString(m.orch.GraphStatus(m.ctx))
		m.appendSystem(strings.TrimRight(b.String(), "\n"))
	case "/resume":
		if m.sending {
			m.appendError(fmt.Errorf("cannot resume a session while a response is in progress"))
			break
		}
		id, err := m.orch.ResumeSession(m.ctx, arg)
		if err != nil {
			m.appendError(err)
			break
		}
		m.appendSystem("resumed session " + id)
	case "/compact":
		if m.sending {
			m.appendError(fmt.Errorf("cannot compact while a response is in progress"))
			break
		}
		if err := m.orch.CompactActiveSession(m.ctx); err != nil {
			m.appendError(err)
			break
		}
		m.appendSystem("session compacted")
	case "/rewind", "/undo":
		path, err := m.orch.UndoLastEdit()
		if err != nil {
			m.appendError(err)
			break
		}
		m.appendSystem("undid last edit: " + path)
	case "/plan":
		switch strings.ToLower(arg) {
		case "", "status":
			if m.orch.PlanMode() {
				m.appendSystem("plan mode on · mutating tools blocked · /plan off to execute")
			} else {
				m.appendSystem("plan mode off · /plan on to plan without edits")
			}
		case "on", "true", "1":
			m.orch.SetPlanMode(true)
			m.appendSystem("plan mode on · agent may not write files or run shell")
		case "off", "false", "0":
			m.orch.SetPlanMode(false)
			m.appendSystem("plan mode off · edits allowed under the usual permission prompt")
		default:
			m.appendError(fmt.Errorf("usage: /plan [on|off]"))
		}
	case "/learn":
		m.handleLearnCommand(arg)
	case "/sandbox":
		m.appendSystem(m.orch.SandboxStatus())
	case "/mouse":
		m.mouseCapture = !m.mouseCapture
		if m.mouseCapture {
			m.statusMsg = "mouse scrolling enabled · shift+drag selects text"
		} else {
			m.statusMsg = "mouse scrolling disabled · drag selects text"
		}
	default:
		m.appendError(fmt.Errorf("unknown command: %s (try /help)", cmd))
	}
	m.setViewportContent(m.renderTranscript())
	m.viewport.GotoBottom()
	return m, nil
}

func (m *appModel) copyText(arg string) (content, okStatus string, err error) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(arg)))
	if len(fields) > 0 && fields[0] == "code" {
		return m.copyCodeBlock(fields[1:])
	}
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "", "last", "response":
		if m.lastAssistantText != "" {
			return m.lastAssistantText, "copied response", nil
		}
		if visible := m.visiblePlainText(); strings.TrimSpace(visible) != "" {
			return visible, "copied visible output", nil
		}
		return "", "", fmt.Errorf("nothing to copy")
	case "visible":
		visible := m.visiblePlainText()
		if strings.TrimSpace(visible) == "" {
			return "", "", fmt.Errorf("nothing to copy")
		}
		return visible, "copied visible output", nil
	case "all", "transcript":
		all := strings.TrimRight(ansi.Strip(m.viewport.GetContent()), "\n")
		if strings.TrimSpace(all) == "" {
			return "", "", fmt.Errorf("nothing to copy")
		}
		return all, "copied transcript", nil
	default:
		return "", "", fmt.Errorf("usage: /copy [last|visible|all|code]")
	}
}

func (m *appModel) copyCodeBlock(args []string) (content, okStatus string, err error) {
	src := m.lastAssistantText
	if src == "" {
		src = m.activeAgentText.String()
	}
	blocks := extractFencedBlocks(src)
	if len(blocks) == 0 {
		return "", "", fmt.Errorf("no code block to copy")
	}
	idx := len(blocks) - 1
	if len(args) > 0 {
		n, parseErr := strconv.Atoi(args[0])
		if parseErr != nil || n < 1 || n > len(blocks) {
			return "", "", fmt.Errorf("code block %s not found (%d in reply)", args[0], len(blocks))
		}
		idx = n - 1
	}
	return blocks[idx], "copied code block", nil
}

func (m *appModel) visiblePlainText() string {
	return strings.TrimRight(ansi.Strip(m.viewport.View()), "\n")
}

func (m *appModel) copyClipboardCmd(content, okStatus string) tea.Cmd {
	m.statusMsg = "copying"
	write := m.clipboardWrite
	return func() tea.Msg {
		if write == nil {
			return clipboardWriteResultMsg{err: fmt.Errorf("clipboard writer unavailable")}
		}
		return clipboardWriteResultMsg{err: write(content), okStatus: okStatus}
	}
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
		fmt.Fprintf(&b, "\nthinking: %s  (change with /think off|low|medium|high)", m.orch.ThinkingLevel())
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

func (m *appModel) handleThinkCommand(arg string) {
	if arg == "" {
		m.appendSystem(fmt.Sprintf("thinking: %s\nset with: /think off|low|medium|high", m.orch.ThinkingLevel()))
		return
	}
	if err := m.orch.SetThinking(arg); err != nil {
		m.appendError(err)
		return
	}
	m.appendSystem(fmt.Sprintf("thinking: %s", m.orch.ThinkingLevel()))
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
		m.setViewportContent(m.renderTranscript())
		m.viewport.GotoBottom()
		return m, listenOAuth(msg.ch)
	}
	if msg.ev.err != nil {
		m.appendError(msg.ev.err)
	} else {
		m.appendSystem("OAuth login complete")
	}
	m.setViewportContent(m.renderTranscript())
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

// handleContextCommand implements /context: model and usage details followed
// by the latest metadata-only context composition report.
func (m *appModel) handleContextCommand() {
	provider, modelID := m.orch.ActiveModelInfo()
	var b strings.Builder
	fmt.Fprintf(&b, "model: %s / %s\n", provider, modelID)
	if info, ok := modelinfo.Lookup(provider, modelID); ok {
		fmt.Fprintf(&b, "context window: %s tokens\n", formatTokenCount(info.ContextWindow))
	} else {
		b.WriteString("context window: unknown (model not in registry)\n")
	}
	fmt.Fprintln(&b, m.usageSummary())
	if status := m.orch.BudgetStatusLine(); status != "" {
		fmt.Fprintln(&b, status)
	}
	if m.lastContextReport == nil {
		b.WriteString("context sources: no context report yet\n")
	} else {
		b.WriteString(RenderContextReport(*m.lastContextReport, m.lastMemoryIntent, m.viewport.Width()))
		b.WriteByte('\n')
	}
	b.WriteString(m.orch.LastRouteStatus() + " · verify:" + string(m.orch.VerificationMode()) + "\n")
	b.WriteString(m.orch.GraphStatus(m.ctx) + "\n")
	if files := m.orch.ProjectInstructionFiles(); len(files) > 0 {
		b.WriteString("project instructions: " + strings.Join(files, ", ") + "\n")
	} else {
		b.WriteString("project instructions: none discovered (AGENTS.md / CLAUDE.md)\n")
	}
	m.appendSystem(strings.TrimRight(b.String(), "\n"))
}

func (m *appModel) handleLearnCommand(arg string) {
	fields := strings.Fields(arg)
	if len(fields) == 0 || fields[0] == "list" {
		pending, err := m.orch.ListPendingSuggestions()
		if err != nil {
			m.appendError(err)
			return
		}
		if len(pending) == 0 {
			m.appendSystem("no pending learning suggestions · run: chronos-code learn suggest")
			return
		}
		var b strings.Builder
		b.WriteString("pending suggestions (accept is review-gated):\n")
		for _, sug := range pending {
			fmt.Fprintf(&b, "  %s  %-8s  %s\n", sug.ID, sug.Kind, sug.Title)
		}
		b.WriteString("use /learn accept <id> or /learn reject <id>")
		m.appendSystem(strings.TrimRight(b.String(), "\n"))
		return
	}
	if len(fields) < 2 {
		m.appendError(fmt.Errorf("usage: /learn [list|accept <id>|reject <id>]"))
		return
	}
	switch fields[0] {
	case "accept":
		if err := m.orch.AcceptSuggestion(fields[1]); err != nil {
			m.appendError(err)
			return
		}
		m.appendSystem("accepted " + fields[1] + " · takes effect on next start")
	case "reject":
		if err := m.orch.RejectSuggestion(fields[1]); err != nil {
			m.appendError(err)
			return
		}
		m.appendSystem("rejected " + fields[1])
	default:
		m.appendError(fmt.Errorf("usage: /learn [list|accept <id>|reject <id>]"))
	}
}

func (m *appModel) usageSummary() string {
	input, output, cacheRead, cacheWrite := m.turnUsageCounts()
	session := m.orch.SessionCost()
	return fmt.Sprintf("last turn: input %d │ cache read %d │ cache write %d │ output %d │ cost %s\nexecution: %d model calls │ %d subagents\nsession: input %d │ cache read %d │ output %d │ cost %s",
		input, cacheRead, cacheWrite, output, m.formatCost(m.lastTurnCost.SpentMicrodollars), m.lastModelCalls, m.lastSubagents,
		session.InputTokens, session.CacheReadTokens, session.OutputTokens,
		m.formatCost(session.SpentMicrodollars))
}

func (m *appModel) usageStatus() string {
	input, output, cacheRead, _ := m.turnUsageCounts()
	status := fmt.Sprintf("in %s · out %s", formatTokenCount64(input), formatTokenCount64(output))
	if cacheRead > 0 {
		status += fmt.Sprintf(" · cache %s", formatTokenCount64(cacheRead))
	}
	status += fmt.Sprintf(" · %d calls", m.lastModelCalls)
	if m.lastSubagents > 0 {
		label := "subagents"
		if m.lastSubagents == 1 {
			label = "subagent"
		}
		status += fmt.Sprintf(" · %d %s", m.lastSubagents, label)
	}
	return status + " · " + m.formatCost(m.lastTurnCost.SpentMicrodollars)
}

func (m *appModel) turnUsageCounts() (input, output, cacheRead, cacheWrite int64) {
	input, output = m.lastTurnCost.InputTokens, m.lastTurnCost.OutputTokens
	cacheRead, cacheWrite = m.lastTurnCost.CacheReadTokens, m.lastTurnCost.CacheCreationTokens
	if input == 0 && output == 0 && cacheRead == 0 && cacheWrite == 0 {
		input = int64(m.lastKnownUsage.UncachedPromptTokens())
		output = int64(m.lastKnownUsage.CompletionTokens)
		cacheRead = int64(m.lastKnownUsage.CacheReadTokens)
		cacheWrite = int64(m.lastKnownUsage.CacheCreationTokens)
	}
	return input, output, cacheRead, cacheWrite
}

func (m *appModel) formatCost(cost budget.Microdollars) string {
	_, modelID := m.orch.ActiveModelInfo()
	if _, err := budget.PriceForModel(modelID); err != nil {
		return "n/a"
	}
	return fmt.Sprintf("$%.6f", float64(cost)/1_000_000)
}

func (m *appModel) transcriptBytes() int {
	total := m.activeAgentText.Len()
	for _, block := range m.blocks {
		total += len(block)
	}
	for _, item := range m.activeTurnItems {
		total += len(item.content)
	}
	return total
}

func formatBytes(n uint64) string {
	const mib = 1024 * 1024
	if n >= mib {
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}

// formatTokenCount renders a token count compactly (e.g. "12.3k", "1.0M")
// for status bar / picker display; non-positive counts (the modelinfo
// "unknown" sentinel) render as "unknown" rather than "0".
func formatTokenCount(n int) string {
	return formatTokenCount64(int64(n))
}

func formatTokenCount64(n int64) string {
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
	used := m.lastKnownUsage.ContextTokens
	if used == 0 {
		used = m.lastKnownUsage.PromptTokens + m.lastKnownUsage.CompletionTokens
	}
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
	if m.signedIn() {
		m.input.Placeholder = "Message chronos-code..."
	} else {
		m.input.Placeholder = "Not signed in — /login or Ctrl+L · Claude Code, Codex, or API key"
	}
	if m.width > 0 {
		m.input.SetWidth(m.width - inputBoxBorderWidth - inputBoxPaddingWidth)
	}
}

func (m *appModel) signedIn() bool {
	if m.orch == nil {
		return false
	}
	candidates := []string{"anthropic", "openai"}
	if provider, _ := m.orch.ActiveModelInfo(); provider != "" {
		candidates = append([]string{provider}, candidates...)
	}
	return len(m.orch.AuthorizedProviders(m.ctx, candidates)) > 0
}

func (m *appModel) sessionIdentitySegment() string {
	if m.orch == nil {
		return ""
	}
	if !m.signedIn() {
		return "not signed in · /login"
	}
	_, modelID := m.orch.ActiveModelInfo()
	if modelID == "" {
		return "signed in"
	}
	return modelID
}

// appendUserTurn, appendSystem and appendError all wrap to m.viewport.Width():
// the viewport itself never wraps long lines, so an unwrapped line can
// overflow into and visually corrupt the fixed-height chrome below it — the
// same class of bug that made the status bar wrap onto a second line (see
// styleHeaderBar's comment in styles.go).
func (m *appModel) appendUserTurn(line string) {
	header := RenderTurnHeader("❯", "you", styleUserPrefix, m.viewport.Width())
	body := wrapText(line, m.viewport.Width())
	m.appendBlock(header + "\n" + body)
	m.setViewportContent(m.renderTranscript())
	m.viewport.GotoBottom()
}

func (m *appModel) appendSystem(s string) {
	m.appendBlock(wrapText(styleDim.Render(s), m.viewport.Width()))
}

func (m *appModel) appendError(err error) {
	m.appendBlock(wrapText(styleError.Render(classifyErrorMessage(err)), m.viewport.Width()))
}

// classifyErrorMessage returns a user-friendly error message. If the error is
// already classified (from the orchestrator retry layer), it uses that message.
// Otherwise it classifies and returns a friendly message.
func classifyErrorMessage(err error) string {
	var classified *apierror.Classified
	if errors.As(err, &classified) {
		return classified.Message
	}
	if c := apierror.Classify(err); c != nil {
		return c.Message
	}
	return "error: " + err.Error()
}

// classifyStatusMessage returns a short status bar label for a failed request.
func classifyStatusMessage(err error) string {
	var classified *apierror.Classified
	if errors.As(err, &classified) {
		return classified.Category.String()
	}
	if c := apierror.Classify(err); c != nil && c.Category != apierror.CategoryUnknown {
		return c.Category.String()
	}
	return "request failed"
}

func (m *appModel) appendBlock(block string) {
	m.blocks = append(m.blocks, block)
	m.blockBytes += len(block)
	if !m.finalizedDirty && m.finalizedCount == len(m.blocks)-1 {
		if m.finalizedText != "" {
			m.finalizedText += "\n\n"
		}
		m.finalizedText += block
		m.finalizedCount++
	} else {
		m.finalizedDirty = true
	}
	trimmed := 0
	for m.blockBytes > maxTranscriptBytes && len(m.blocks) > 1 {
		m.blockBytes -= len(m.blocks[0])
		m.blocks = m.blocks[1:]
		m.trimmedBlocks++
		trimmed++
	}
	if trimmed > 0 {
		m.finalizedText = ""
		m.finalizedCount = 0
		m.finalizedDirty = true
		if m.hasLastTurn {
			m.lastTurnBlockIdx -= trimmed
			if m.lastTurnBlockIdx < 0 {
				m.hasLastTurn = false
				m.lastTurnItems = nil
			}
		}
	}
}

// finalizeTurn closes out the in-progress agent turn (streamed or not),
// folding the ordered active turn into a permanent transcript block
// and resetting the in-progress state. Failed turns retain their activity
// timeline before the error so the user can see what led to the failure. One
// queued follow-up is dispatched after the active turn has fully settled.
func (m *appModel) finalizeTurn(err error) tea.Cmd {
	interrupted := m.turnInterrupted && (err == nil || errors.Is(err, context.Canceled))
	budgetExhausted := err != nil && strings.Contains(err.Error(), "token budget exceeded for session")
	if interrupted {
		err = nil
	}
	if budgetExhausted && !m.budgetRetried && m.activeRequest != "" {
		if cmd, resetErr := m.retryInFreshSession(); resetErr == nil {
			return cmd
		} else {
			err = fmt.Errorf("%w; automatic session rollover failed: %v", err, resetErr)
		}
	}
	m.settleTurnActivities(err)
	m.sending = false
	for i := range m.activeTurnItems {
		if m.activeTurnItems[i].kind == turnItemText {
			m.activeTurnItems[i].rendered = ""
		}
	}
	if m.turnCancel != nil {
		m.turnCancel()
		m.turnCancel = nil
		m.turnCtx = nil
	}
	if m.stopActivity != nil {
		m.stopActivity()
		m.stopActivity = nil
		m.activityCh = nil
	}
	m.lastTurnItems = cloneTurnItems(m.activeTurnItems)
	m.lastTurnErr = err
	m.lastTurnInterrupted = interrupted
	m.appendBlock(m.buildAssistantBlock(m.lastTurnItems, interrupted, err))
	m.hasLastTurn = true
	m.lastTurnBlockIdx = len(m.blocks) - 1
	if err == nil {
		m.lastAssistantText = m.activeAgentText.String()
	}
	if m.lastUsage.PromptTokens > 0 || m.lastUsage.CompletionTokens > 0 || m.lastUsage.CacheReadTokens > 0 || m.lastUsage.CacheCreationTokens > 0 {
		m.lastKnownUsage = m.lastUsage
		m.statusMsg = fmt.Sprintf("tokens: %d prompt + %d cache read + %d completion",
			m.lastUsage.UncachedPromptTokens(), m.lastUsage.CacheReadTokens, m.lastUsage.CompletionTokens)
	}
	cost := m.orch.SessionCost()
	turnCost := budget.SessionCost{
		InputTokens:         cost.InputTokens - m.turnCostStart.InputTokens,
		OutputTokens:        cost.OutputTokens - m.turnCostStart.OutputTokens,
		CacheReadTokens:     cost.CacheReadTokens - m.turnCostStart.CacheReadTokens,
		CacheCreationTokens: cost.CacheCreationTokens - m.turnCostStart.CacheCreationTokens,
		SpentMicrodollars:   cost.SpentMicrodollars - m.turnCostStart.SpentMicrodollars,
	}
	if turnCost.InputTokens > 0 || turnCost.OutputTokens > 0 || turnCost.CacheReadTokens > 0 || turnCost.CacheCreationTokens > 0 {
		m.lastTurnCost = turnCost
		m.lastModelCalls = m.turnModelCalls
		m.lastSubagents = m.turnSubagents
	}
	if interrupted {
		m.statusMsg = "interrupted"
	} else if err != nil {
		if budgetExhausted {
			m.statusMsg = "budget exhausted │ /clear to continue"
		} else {
			m.statusMsg = classifyStatusMessage(err) + " · " + m.usageStatus()
		}
	} else {
		m.statusMsg = m.usageStatus()
	}
	m.turnInterrupted = false
	m.activeAgentText.Reset()
	m.activeTurnItems = nil
	m.activityIndex = nil
	m.activityArgs = nil
	m.pendingToolCalls = 0
	m.pendingSubagents = 0
	m.lastChunk = ""
	m.activeRequest = ""
	m.activeSkill = ""
	m.budgetRetried = false
	m.lastUsage = model.Usage{}
	m.setViewportContent(m.renderTranscript())
	if m.followOutput {
		m.viewport.GotoBottom()
	}

	if budgetExhausted || len(m.queuedMessages) == 0 {
		return nil
	}
	queued := m.queuedMessages[0]
	m.queuedMessages = m.queuedMessages[1:]
	_, cmd := m.handleSubmit(queued)
	return cmd
}

func (m *appModel) settleTurnActivities(err error) {
	for i := range m.activeTurnItems {
		if m.activeTurnItems[i].kind != turnItemActivity {
			continue
		}
		if err == nil {
			m.activeTurnItems[i].content = strings.ReplaceAll(m.activeTurnItems[i].content, "· working", "· completed")
			m.activeTurnItems[i].content = strings.ReplaceAll(m.activeTurnItems[i].content, "· running", "· done")
		} else {
			m.activeTurnItems[i].content = strings.ReplaceAll(m.activeTurnItems[i].content, "· working", "· failed")
			m.activeTurnItems[i].content = strings.ReplaceAll(m.activeTurnItems[i].content, "· running", "· failed")
		}
	}
}

func (m *appModel) retryInFreshSession() (tea.Cmd, error) {
	if m.turnCancel != nil {
		m.turnCancel()
	}
	if m.stopActivity != nil {
		m.stopActivity()
		m.stopActivity = nil
		m.activityCh = nil
	}

	// Prefer compacting the existing session over discarding it: a budget
	// cap is a cumulative-cost concern, not a context-window concern, so the
	// conversation itself is usually still small and worth keeping. Only
	// fall back to a brand-new, empty session if compaction itself fails
	// (e.g. the summarizer call errors) — recovering with a clean slate
	// beats getting stuck unable to recover at all.
	activityLine := "  ↻ session budget reached · compacting history and resuming"
	statusMsg := "session compacted after budget limit"
	if compactErr := m.orch.CompactActiveSession(m.ctx); compactErr != nil {
		if _, err := m.orch.ResetSession(m.ctx); err != nil {
			return nil, err
		}
		activityLine = "  ↻ session budget reached · continuing in a fresh session"
		statusMsg = "session renewed after budget limit"
	}
	m.turnCostStart = m.orch.SessionCost()
	m.lastKnownUsage = model.Usage{}
	m.lastTurnCost = budget.SessionCost{}
	m.budgetRetried = true
	m.turnID++
	m.turnCtx, m.turnCancel = context.WithCancel(m.ctx)
	if m.activeSkill != "" {
		ctx, err := m.orch.WithSkill(m.turnCtx, m.activeSkill)
		if err != nil {
			return nil, err
		}
		m.turnCtx = ctx
	}
	m.lastUsage = model.Usage{}
	m.lastChunk = ""
	m.pendingToolCalls = 0
	m.pendingSubagents = 0
	m.activityIndex = make(map[string]int)
	m.activityArgs = make(map[string]any)
	m.appendTurnActivity(styleDim.Render(activityLine))
	m.statusMsg = statusMsg
	turnID := m.turnID
	turnCtx := m.turnCtx
	var activityCmd tea.Cmd
	if ch, stop, subscribeErr := m.orch.SubscribeActivity(); subscribeErr == nil {
		m.activityCh = ch
		m.stopActivity = stop
		activityCmd = listenActivity(turnCtx, turnID, ch)
	}
	m.refreshViewport()
	return tea.Batch(m.sendCmd(turnCtx, turnID, m.activeRequest), m.spin.Tick, activityCmd), nil
}

func (m *appModel) renderTranscript() string {
	m.transcriptBuf.Reset()
	m.transcriptBuf.WriteString(m.renderFinalizedTranscript())

	if m.sending {
		if m.transcriptBuf.Len() > 0 {
			m.transcriptBuf.WriteString("\n\n")
		}
		m.transcriptBuf.WriteString(RenderTurnHeader("✦", m.displayAgentName(), styleAgentName, m.viewport.Width()))
		m.transcriptBuf.WriteByte('\n')
		if len(m.activeTurnItems) > 0 {
			m.transcriptBuf.WriteString(m.renderTurnItems())
		} else {
			m.transcriptBuf.WriteString(styleDim.Render(m.spin.View() + " thinking..."))
		}
	}

	return m.transcriptBuf.String()
}

func (m *appModel) renderFinalizedTranscript() string {
	if !m.finalizedDirty && m.finalizedCount == len(m.blocks) {
		return m.finalizedText
	}
	var b strings.Builder
	if m.trimmedBlocks > 0 {
		fmt.Fprintf(&b, "%s\n\n", styleDim.Render(fmt.Sprintf("[%d older transcript blocks omitted]", m.trimmedBlocks)))
	}
	for i, block := range m.blocks {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(block)
	}
	m.finalizedText = b.String()
	m.finalizedCount = len(m.blocks)
	m.finalizedDirty = false
	return m.finalizedText
}

func (m *appModel) toggleToolDetails() (tea.Model, tea.Cmd) {
	m.toolsExpanded = !m.toolsExpanded
	if m.toolsExpanded {
		m.statusMsg = "tool details expanded · ctrl+o collapses"
	} else {
		m.statusMsg = "tool details collapsed · ctrl+o expands"
	}
	if !m.sending {
		m.rewriteLastTurnBlock()
	}
	if m.ready {
		m.refreshViewport()
	}
	return m, nil
}

func (m *appModel) rewriteLastTurnBlock() {
	if !m.hasLastTurn || m.lastTurnBlockIdx < 0 || m.lastTurnBlockIdx >= len(m.blocks) {
		return
	}
	next := m.buildAssistantBlock(m.lastTurnItems, m.lastTurnInterrupted, m.lastTurnErr)
	prev := m.blocks[m.lastTurnBlockIdx]
	m.blockBytes -= len(prev)
	m.blocks[m.lastTurnBlockIdx] = next
	m.blockBytes += len(next)
	m.finalizedDirty = true
}

func cloneTurnItems(items []turnItem) []turnItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]turnItem, len(items))
	copy(out, items)
	return out
}

func turnHasText(items []turnItem) bool {
	for _, item := range items {
		if item.kind == turnItemText && item.content != "" {
			return true
		}
	}
	return false
}

func (m *appModel) buildAssistantBlock(items []turnItem, interrupted bool, err error) string {
	var b strings.Builder
	b.WriteString(RenderTurnHeader("✦", m.displayAgentName(), styleAgentName, m.viewport.Width()))
	if err != nil {
		b.WriteByte('\n')
		if len(items) > 0 {
			b.WriteString(m.renderItemList(items))
			b.WriteString("\n\n")
		}
		message := classifyErrorMessage(err)
		if strings.Contains(err.Error(), "token budget exceeded for session") {
			message += "\n\nThis session has reached its cumulative token limit. Use /clear to start a fresh session."
		}
		b.WriteString(wrapText(styleError.Render(message), m.viewport.Width()))
		return b.String()
	}
	b.WriteString("\n")
	if interrupted && !turnHasText(items) {
		b.WriteString(styleDim.Render("interrupted"))
		return b.String()
	}
	b.WriteString(m.renderItemList(items))
	return b.String()
}

func activityNeedsPeek(content string) bool {
	return strings.Contains(content, "· running") ||
		strings.Contains(content, "· working") ||
		strings.Contains(content, "· failed")
}

func (m *appModel) renderTurnItems() string {
	return m.renderItemList(m.activeTurnItems)
}

func (m *appModel) renderItemList(items []turnItem) string {
	if m.toolsExpanded {
		return m.renderExpandedItems(items)
	}
	return m.renderCollapsedItems(items)
}

func (m *appModel) renderExpandedItems(items []turnItem) string {
	var b strings.Builder
	for i := range items {
		item := &items[i]
		if i > 0 {
			if item.kind == turnItemText || items[i-1].kind == turnItemText {
				b.WriteString("\n\n")
			} else {
				b.WriteByte('\n')
			}
		}
		b.WriteString(m.renderOneItem(item))
	}
	return b.String()
}

func (m *appModel) renderCollapsedItems(items []turnItem) string {
	var b strings.Builder
	i := 0
	wrote := false
	for i < len(items) {
		if items[i].kind == turnItemText {
			if wrote {
				b.WriteString("\n\n")
			}
			b.WriteString(m.renderOneItem(&items[i]))
			wrote = true
			i++
			continue
		}
		j := i
		for j < len(items) && items[j].kind == turnItemActivity {
			j++
		}
		if wrote {
			if i > 0 && items[i-1].kind == turnItemText {
				b.WriteString("\n\n")
			} else {
				b.WriteByte('\n')
			}
		}
		b.WriteString(m.renderActivityRun(items[i:j]))
		wrote = true
		i = j
	}
	return b.String()
}

func (m *appModel) renderActivityRun(run []turnItem) string {
	if len(run) <= 1 {
		return m.renderOneItem(&run[0])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %s %s %s",
		styleTool.Render("▸"),
		styleBold.Render(fmt.Sprintf("%d tool calls", len(run))),
		styleDim.Render("· ctrl+o expand"))
	seenLast := false
	for i := range run {
		if activityNeedsPeek(run[i].content) || i == len(run)-1 {
			if i == len(run)-1 {
				seenLast = true
			}
			b.WriteByte('\n')
			b.WriteString(m.renderOneItem(&run[i]))
		}
	}
	if !seenLast {
		b.WriteByte('\n')
		b.WriteString(m.renderOneItem(&run[len(run)-1]))
	}
	return b.String()
}

func (m *appModel) renderOneItem(item *turnItem) string {
	if item.kind == turnItemActivity {
		return truncateToWidth(item.content, m.viewport.Width())
	}
	width := m.viewport.Width()
	if item.rendered != "" && item.renderedWidth == width {
		return item.rendered
	}
	if m.sending {
		item.rendered = wrapText(item.content, width)
	} else {
		item.rendered = RenderMarkdownLite(item.content, width)
	}
	item.renderedWidth = width
	return item.rendered
}

func progressLabel(toolCalls, subagents int, state string) string {
	count, noun := toolCalls, "tool calls"
	if subagents > 0 {
		count, noun = subagents, "subagents"
		if subagents == 1 {
			noun = "subagent"
		}
	} else if toolCalls == 1 {
		noun = "tool call"
	}
	return fmt.Sprintf("%d %s %s", count, noun, state)
}

func (m *appModel) invalidateRenderCache() {
	m.finalizedDirty = true
	m.finalizedText = ""
	m.finalizedCount = 0
	for i := range m.activeTurnItems {
		m.activeTurnItems[i].rendered = ""
	}
}

func (m *appModel) transcriptView() string {
	if m.viewportViewValid {
		return m.viewportViewCache
	}
	m.viewportViewCache = m.viewport.View()
	m.viewportViewValid = true
	return m.viewportViewCache
}

func (m *appModel) View() tea.View {
	defer m.perf.recordViewEnd()
	if !m.ready {
		return tea.View{AltScreen: true}
	}

	view := tea.View{
		Content:   lipgloss.JoinVertical(lipgloss.Left, m.renderHeaderBar(), m.transcriptView(), m.bottomView, m.renderStatusBar()),
		AltScreen: true,
	}
	if m.mouseCapture {
		view.MouseMode = tea.MouseModeCellMotion
	}
	return view
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
	if m.width <= 0 {
		return ""
	}
	agentID := ""
	if m.orch != nil {
		agentID = m.orch.ActiveID()
	}
	if m.headerCache != "" && m.headerCacheWidth == m.width && m.headerCacheAgent == agentID && m.headerCacheDir == m.workDir {
		return m.headerCache
	}
	left := " ◆ chronos-code "
	if m.orch != nil && m.orch.ActiveID() != m.orch.PrimaryID() {
		left = " ◆ chronos-code · @" + m.orch.ActiveID() + " "
	}
	right := ""
	if m.workDir != "" {
		dir := m.workDir
		if m.homeDir != "" {
			if rel, err := filepath.Rel(m.homeDir, dir); err == nil && !strings.HasPrefix(rel, "..") {
				dir = "~/" + rel
			}
		}
		right = " " + dir + " "
	}
	if lipgloss.Width(left) >= m.width {
		out := styleHeaderBar.Render(truncateToWidth(left, m.width))
		m.storeHeaderCache(agentID, out)
		return out
	}
	right = truncateToWidth(right, m.width-lipgloss.Width(left))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	out := styleHeaderBar.Render(left + strings.Repeat(" ", gap) + right)
	m.storeHeaderCache(agentID, out)
	return out
}

func (m *appModel) storeHeaderCache(agentID, rendered string) {
	m.headerCache = rendered
	m.headerCacheWidth = m.width
	m.headerCacheAgent = agentID
	m.headerCacheDir = m.workDir
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
	if m.width <= 0 {
		return ""
	}
	streamLabel := "batch"
	if m.stream {
		streamLabel = "stream"
	}
	runLabel := "idle"
	if m.sending {
		runLabel = "running"
		if m.turnInterrupted {
			runLabel = "stopping"
		}
	}
	leftText := " ● " + runLabel + " │ " + streamLabel
	if m.orch.ActiveID() != m.orch.PrimaryID() {
		leftText = " ● " + runLabel + " │ @" + m.orch.ActiveID() + " │ " + streamLabel
	}
	if m.width >= 100 {
		if m.orch.PlanMode() {
			leftText += " │ plan"
		}
		if think := m.orch.ThinkingLevel(); think != "off" {
			leftText += " │ think:" + think
		}
		leftText += " │ " + string(m.orch.VerificationMode())
		if route := m.orch.LastRouteStatus(); route != "route:—" {
			leftText += " │ " + route
		}
	}
	if ident := m.sessionIdentitySegment(); ident != "" {
		leftText += " │ " + ident
	}
	if ctxSeg := m.contextUsageSegment(); ctxSeg != "" {
		leftText += " │ " + ctxSeg
	}
	if !m.followOutput {
		leftText += fmt.Sprintf(" │ scrolled %d%% · ctrl+end follow", int(m.viewport.ScrollPercent()*100))
	}
	if len(m.queuedMessages) > 0 {
		leftText += fmt.Sprintf(" │ queued %d", len(m.queuedMessages))
	}
	leftText += " "
	if m.width < 72 {
		leftText = " ● " + runLabel
		if len(m.queuedMessages) > 0 {
			leftText += fmt.Sprintf(" +%d", len(m.queuedMessages))
		}
		if !m.followOutput {
			leftText += " ↑"
		}
		leftText += " "
	}
	leftText = truncateToWidth(leftText, m.width)
	leftSeg := styleStatusLeft.Render(leftText)

	rightText := " drag-select copy │ ctrl+shift+c last │ ctrl+/ commands │ ctrl+c interrupt/quit "
	if m.mouseCapture {
		rightText = " shift+drag copy │ ctrl+shift+c last │ ctrl+/ commands │ ctrl+c interrupt/quit "
	}
	if m.statusMsg != "" {
		rightText = " " + m.statusMsg + " │" + rightText
	}
	if m.width < 90 {
		rightText = " " + m.statusMsg + " "
	}
	rightSeg := styleStatusRight.Render(rightText)
	if lipgloss.Width(leftSeg)+lipgloss.Width(rightSeg) > m.width {
		rightText = " " + m.statusMsg + " "
		available := m.width - lipgloss.Width(leftSeg)
		if available < 0 {
			available = 0
		}
		rightSeg = styleStatusRight.Render(truncateToWidth(rightText, available))
	}

	gap := m.width - lipgloss.Width(leftSeg) - lipgloss.Width(rightSeg)
	if gap < 0 {
		gap = 0
	}
	return leftSeg + styleStatusFill.Render(strings.Repeat(" ", gap)) + rightSeg
}

func removeLastRune(s string) string {
	_, size := utf8.DecodeLastRuneInString(s)
	if size == 0 {
		return s
	}
	return s[:len(s)-size]
}
