package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	"github.com/spawn08/chronos-code/internal/execution"
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
	maxViewportLines     = 2000
	maxRenderBytes       = 256 << 10
	maxItemRenderBytes   = 64 << 10
	maxInspectionBytes   = 1 << 20
	maxShellOutputLines  = 200
	maxShellOutputBytes  = 64 << 10
	authIdentityTTL      = 30 * time.Second
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
	attachments   string
	completion    <-chan orchestrator.ExecutionCompletion
	execution     orchestrator.ExecutionResult
}

type streamDeltaMsg struct {
	turnID     uint64
	ctx        context.Context
	resp       *model.ChatResponse
	ch         <-chan *model.ChatResponse
	completion <-chan orchestrator.ExecutionCompletion
}

type streamDoneMsg struct {
	turnID     uint64
	err        error
	completion *orchestrator.ExecutionCompletion
}

type operationalSnapshotMsg struct {
	snapshot orchestrator.OperationalSnapshot
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
	turnItemReceipt
)

type turnItem struct {
	kind          turnItemKind
	content       string
	rendered      string
	renderedWidth int
	text          *strings.Builder
	activity      activityKind
	toolName      string
	callID        string
	agentID       string
	args          string
	result        string
	failure       string
	started       time.Time
	duration      time.Duration
	observedTime  bool
	settled       bool
}

type activityKind uint8

const (
	activityTool activityKind = iota
	activityModel
	activityRetry
	activityContext
	activityThinking
	activityProgress
)

// Raw source is retained separately from the width-dependent block cache.
// Both caches are bounded; the latest complete assistant text also serves copy.
type transcriptSource struct {
	user        string
	items       []turnItem
	name        string
	err         error
	interrupted bool
	bytes       int
	width       int
}

// chatDoneMsg carries the result of a non-streaming orch.Chat call.
type chatDoneMsg struct {
	turnID        uint64
	resp          *model.ChatResponse
	contextReport orchestrator.ContextReport
	memoryIntent  *memory.IntentResult
	attachments   string
	err           error
	result        orchestrator.ExecutionResult
}

type maintenanceDoneMsg struct {
	turnID uint64
	text   string
	err    error
}

type subagentDoneMsg struct {
	turnID uint64
	name   string
	result string
	err    error
}

type shellDoneMsg struct {
	output string
	err    error
}

// modelPickerLiveMsg carries the results of asynchronously fetching every
// currently authorized, live-listing-capable provider's real model list
// (Orchestrator.ListProviderModels) to upgrade an already-open Ctrl+M model
// picker in place — e.g. so Azure's actual deployments replace the three
// generic example deployment names the picker opened with. This covers
// every authorized provider, not just the active one, since Ctrl+M is
// often opened specifically to discover a provider's real model names
// *before* switching to it — gating this on the active provider would make
// it useless for exactly that case.
type modelPickerLiveMsg struct {
	results []providerModelsResult
}

type providerModelsResult struct {
	provider string
	models   []modelinfo.Info
	ok       bool
}

// fetchModelPickerLiveCmd fires the same live model list request
// handleModelCommand's bare /model uses for the active provider, but
// against every currently authorized provider modelinfo.LiveProviders
// supports. Requests run concurrently, each bounded to 5s, so one
// slow/unreachable provider can't hold up the others; opening the picker
// itself (newModelPicker) stays synchronous and instant, and results land
// later as a single modelPickerLiveMsg.
func fetchModelPickerLiveCmd(ctx context.Context, orch *orchestrator.Orchestrator) tea.Cmd {
	return func() tea.Msg {
		candidates := orch.AuthorizedProviders(ctx, modelinfo.LiveProviders())
		results := make([]providerModelsResult, len(candidates))
		var wg sync.WaitGroup
		for i, provider := range candidates {
			wg.Add(1)
			go func(i int, provider string) {
				defer wg.Done()
				fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				models, ok := orch.ListProviderModels(fetchCtx, provider)
				results[i] = providerModelsResult{provider: provider, models: models, ok: ok}
			}(i, provider)
		}
		wg.Wait()
		return modelPickerLiveMsg{results: results}
	}
}

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
	blockSources        []*transcriptSource
	rawBlockBytes       int
	blockBytes          int
	trimmedBlocks       int
	finalizedText       string
	finalizedDirty      bool
	finalizedCount      int
	activeAgentText     strings.Builder
	activeTurnItems     []turnItem
	activityIndex       map[string]int
	activityArgs        map[string]any
	activityDetailBytes int
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
	lastExecution       orchestrator.ExecutionSnapshot
	lastTurnBlockIdx    int
	hasLastTurn         bool
	toolsExpanded       bool
	activeRequest       string // Original request only; never replayed after failure.
	budgetRetried       bool   // Legacy state retained for compatibility; no whole-task retry.
	lastUsage           model.Usage
	streamFinalReceived bool
	streamStopReason    model.StopReason
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
	// mouseCapture defaults to true so the wheel scrolls the alt-screen
	// transcript. Unshifted drag-select is impossible while this is on;
	// copy uses shift+drag, Ctrl+Shift+C, and /copy. /mouse is an opt-out
	// for unshifted terminal selection — do not default this to false to
	// "fix copy", that regresses scrolling.
	mouseCapture bool
	bottomView   string
	bottomModal  bool

	approval   *pendingApproval
	wizard     *loginWizard
	picker     *picker
	inspection *inspectionOverlay

	queuedMessages []string
	pastedInputs   map[string]string
	pasteID        uint64

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

	authCheckedAt time.Time
	authSignedIn  bool
	authModelID   string
	authCatalog   []string
	authCatalogAt time.Time
	operational   orchestrator.OperationalSnapshot

	quitting bool
}

// RunTUI replaces the old bufio.Scanner-based REPL (NewREPL/Start) with a
// bubbletea program: scrollback viewport, multi-line input with history,
// markdown-lite response rendering, and a modal-based permission prompt that
// doesn't fight bubbletea for stdin the way a second bufio.Reader would.
func RunTUI(orch *orchestrator.Orchestrator, stream bool) error {
	restoreLogs, err := redirectTUILogs()
	if err != nil {
		return err
	}
	defer restoreLogs()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ta := newComposer()
	ta.Focus()

	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()

	history, historyErr := newPersistentHistory()
	if historyErr != nil {
		history = NewHistory()
	}
	m := &appModel{
		orch:           orch,
		stream:         stream,
		ctx:            ctx,
		cancel:         cancel,
		input:          ta,
		spin:           spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		history:        history,
		clipboardRead:  clipboard.ReadAll,
		clipboardWrite: clipboard.WriteAll,
		mouseCapture:   true,
		workDir:        wd,
		homeDir:        home,
		followOutput:   true,
		statusMsg:      orch.StartupHints(ctx),
	}
	if historyErr != nil {
		m.statusMsg = "command history unavailable: " + historyErr.Error()
	}

	p := tea.NewProgram(m)
	installApprovalHandlers(orch, NewApprovalHandler(p))

	_, err = p.Run()
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
	return tea.Batch(textarea.Blink, m.input.Focus(), m.operationalSnapshotCmd(0))
}

func (m *appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.perf.recordUpdateStart()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		widthChanged := m.width != msg.Width
		m.width, m.height = msg.Width, msg.Height
		contentWidth := msg.Width
		if contentWidth < 1 {
			contentWidth = 1
		}
		if widthChanged {
			m.invalidateRenderCache()
		}
		if m.inspection != nil {
			m.inspection.resize(msg.Width, m.inspectionHeight())
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
		m.refreshViewport()
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.MouseWheelMsg:
		if !m.ready || !m.mouseCapture {
			return m, nil
		}
		if m.inspection != nil && m.approval == nil {
			var cmd tea.Cmd
			m.inspection.viewport, cmd = m.inspection.viewport.Update(msg)
			return m, cmd
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		m.afterViewportScroll()
		return m, cmd

	case tea.PasteMsg:
		if m.approval != nil || m.wizard != nil || m.picker != nil || m.inspection != nil || m.searching {
			return m, nil
		}
		m.insertPaste(msg.Content)
		m.resizeViewport()
		return m, nil

	case spinner.TickMsg:
		if !m.sending {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		// The spinner is only visible before the first turn item. Continuing
		// the tick loop after that forces a full-frame View ~10Hz for no gain.
		if !m.followOutput || len(m.activeTurnItems) > 0 {
			return m, nil
		}
		return m, cmd

	case approvalRequestMsg:
		m.approval = &pendingApproval{toolName: msg.toolName, args: msg.args, resp: msg.resp}
		m.resizeViewport()
		return m, nil

	case streamStartedMsg:
		if msg.turnID != m.turnID || !m.sending {
			return m, nil
		}
		m.captureAttachmentReceipt(msg.attachments)
		if msg.ctx.Err() != nil {
			return m, m.finalizeTurn(msg.ctx.Err())
		}
		m.captureExecutionMetadata(msg.contextReport, msg.memoryIntent)
		m.lastExecution = executionSnapshot(msg.execution, true)
		return m, listenStream(msg.ctx, msg.turnID, msg.ch, msg.completion)

	case streamDeltaMsg:
		return m.handleStreamDelta(msg)

	case streamRenderTickMsg:
		m.renderScheduled = false
		// A frozen viewport is required for drag-select / copy: replacing
		// content every tick wipes the terminal selection.
		if m.followOutput {
			m.refreshViewport()
		}
		return m, nil

	case activityMsg:
		return m.handleActivity(msg)

	case activityDoneMsg:
		return m, nil

	case streamDoneMsg:
		if msg.turnID != m.turnID || !m.sending {
			return m, nil
		}
		if msg.completion != nil {
			m.lastExecution = completionSnapshot(m.lastExecution, *msg.completion)
			m.operational.Execution = m.lastExecution
			if msg.err == nil {
				msg.err = completionError(*msg.completion)
			}
		}
		if msg.err == nil {
			if m.turnCtx != nil && m.turnCtx.Err() != nil {
				msg.err = m.turnCtx.Err()
			} else if !m.streamFinalReceived {
				msg.err = fmt.Errorf("response stream ended before completion: %w", io.ErrUnexpectedEOF)
			} else {
				msg.err = responseStopError(m.streamStopReason)
			}
		}
		if msg.err != nil && m.lastExecution.Running {
			m.lastExecution.Running = false
			m.lastExecution.StopReason = execution.StopReasonForError(msg.err)
			m.operational.Execution = m.lastExecution
		}
		return m, m.finalizeTurn(msg.err)

	case chatDoneMsg:
		if msg.turnID != m.turnID || !m.sending {
			return m, nil
		}
		m.captureAttachmentReceipt(msg.attachments)
		m.lastExecution = executionSnapshot(msg.result, false)
		m.operational.Execution = m.lastExecution
		if m.turnCtx != nil && m.turnCtx.Err() != nil {
			m.lastExecution.StopReason = execution.StopCancelled
			return m, m.finalizeTurn(m.turnCtx.Err())
		}
		m.captureExecutionMetadata(msg.contextReport, msg.memoryIntent)
		if msg.err == nil {
			msg.err = resultError(msg.result)
		}
		if msg.resp != nil {
			if m.activityCh == nil {
				for _, tc := range msg.resp.ToolCalls {
					m.appendTurnActivity(RenderToolCall(tc.Name, SummarizeArgs(tc.Arguments)))
					m.setLastToolMetadata(tc.Name, tc.ID, tc.Arguments)
				}
			}
			m.appendTurnText(msg.resp.Content)
			m.lastUsage = msg.resp.Usage
			if msg.err == nil {
				msg.err = msg.resp.Err
			}
			if msg.err == nil {
				msg.err = responseStopError(msg.resp.StopReason)
			}
		}
		return m, m.finalizeTurn(msg.err)

	case operationalSnapshotMsg:
		m.operational = msg.snapshot
		if m.inspection != nil {
			m.inspection.resize(m.width, m.inspectionHeight())
		}
		m.resizeViewport()
		return m, m.operationalSnapshotCmd(time.Second)

	case maintenanceDoneMsg:
		if msg.turnID != m.turnID || !m.sending {
			return m, nil
		}
		if m.turnCancel != nil {
			m.turnCancel()
		}
		m.turnCtx, m.turnCancel = nil, nil
		m.sending = false
		m.turnInterrupted = false
		m.statusMsg = ""
		if msg.err != nil {
			m.appendError(msg.err)
		} else {
			m.appendSystem(msg.text)
		}
		m.refreshViewport()
		if len(m.queuedMessages) > 0 {
			line := m.queuedMessages[0]
			m.queuedMessages = m.queuedMessages[1:]
			return m.handleSubmit(line)
		}
		return m, nil

	case subagentDoneMsg:
		if msg.turnID != m.turnID {
			return m, nil
		}
		if idx, ok := m.activityIndex["direct-subagent"]; ok {
			m.activeTurnItems[idx].content = RenderToolActivity("", "subagent:"+msg.name, m.activityArgs["direct-subagent"], true, msg.err)
			m.activeTurnItems[idx].result = m.captureActivityValue(msg.result)
			m.activeTurnItems[idx].failure = m.captureActivityValue(msg.err)
		}
		m.pendingToolCalls = 0
		m.pendingSubagents = 0
		if msg.err == nil {
			m.appendTurnText(msg.result)
		}
		return m, m.finalizeTurn(msg.err)

	case shellDoneMsg:
		if msg.output != "" {
			m.appendSystem(truncateShellOutput(msg.output))
		}
		if msg.err != nil {
			m.appendError(msg.err)
		}
		m.statusMsg = ""
		m.refreshViewport()
		return m, nil

	case modelPickerLiveMsg:
		if m.picker != nil && m.picker.isModelPicker {
			for _, r := range msg.results {
				if r.ok && len(r.models) > 0 {
					m.picker.all = mergeLiveModelPickerItems(m.picker.all, r.provider, r.models)
				}
			}
			m.picker.applyFilter()
		}
		return m, nil

	case sessionPickerMsg:
		if m.picker != msg.picker || m.turnID != msg.turnID {
			return m, nil
		}
		p := m.picker
		p.loading = false
		if msg.err != nil {
			p.heading = "Sessions: " + msg.err.Error()
		} else {
			p.all = append(p.all, msg.items...)
			for id, detail := range msg.details {
				p.details[id] = detail
			}
			p.offset += msg.count
			p.more = msg.count == sessionPageSize && p.offset < maxPickerSessions
			p.heading = fmt.Sprintf("Sessions (%d loaded):", len(p.all))
			if p.offset >= maxPickerSessions {
				p.heading = fmt.Sprintf("Latest %d sessions:", maxPickerSessions)
			}
			p.applyFilter()
		}
		m.resizeViewport()
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
		m.insertPaste(msg.content)
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
	if m.inspection != nil {
		return m.handleInspectionKey(msg)
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
		m.afterViewportScroll()
		return m, cmd
	}
	if m.ready && msg.Mod.Contains(tea.ModCtrl) && (msg.Code == tea.KeyUp || msg.Code == tea.KeyDown) {
		if msg.Code == tea.KeyUp {
			m.viewport.HalfPageUp()
		} else {
			m.viewport.HalfPageDown()
		}
		m.afterViewportScroll()
		return m, nil
	}
	if m.ready && msg.Mod.Contains(tea.ModCtrl) && (msg.Code == tea.KeyHome || msg.Code == tea.KeyEnd) {
		if msg.Code == tea.KeyHome {
			m.viewport.GotoTop()
			m.followOutput = false
			m.viewportViewValid = false
		} else {
			m.followOutput = true
			m.refreshViewport()
		}
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
		line := m.expandPastes(strings.TrimSpace(m.input.Value()))
		if line == "" {
			return m, nil
		}
		m.input.Reset()
		m.completionIdx = 0
		m.resizeViewport()
		if strings.Fields(line)[0] == "/inspect" || line == "/session list" {
			return m.handleSubmit(line)
		}
		if m.sending {
			m.queuedMessages = append([]string{line}, m.queuedMessages...)
			m.interruptTurn()
			return m, nil
		}
		return m.handleSubmit(line)
	case msg.String() == "alt+enter" && m.sending:
		line := m.expandPastes(strings.TrimSpace(m.input.Value()))
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
		return m, fetchModelPickerLiveCmd(m.ctx, m.orch)
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
	operationalHeight := 0
	if operational := m.renderOperationalBar(); operational != "" {
		operationalHeight = lipgloss.Height(operational)
	}
	height := m.height - headerHeight - bottomHeight - statusHeight - operationalHeight
	if height < 0 {
		return 0
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
			if m.followOutput {
				m.viewport.GotoBottom()
			}
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
			return joinLayout(m.renderCommandCompletions(completions), input), false
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
			m.history.Add(displayLine)
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
	if m.picker != nil && m.picker.isSessionPicker {
		if m.picker.cancel != nil {
			m.picker.cancel()
		}
		m.picker = nil
	}
	m.history.Add(displayLine)
	m.followOutput = true
	m.appendUserTurn(displayLine)
	m.refreshPrompt()

	m.sending = true
	m.activeRequest = line
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
	m.activityDetailBytes = 0
	m.activityIndex = make(map[string]int)
	m.activityArgs = make(map[string]any)
	m.turnModelCalls = 0
	m.turnSubagents = 0
	m.lastChunk = ""
	m.streamFinalReceived = false
	m.streamStopReason = ""
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
	m.followOutput = true
	m.appendUserTurn(line)
	m.sending = true
	m.activeRequest = ""
	m.budgetRetried = false
	m.turnID++
	m.turnCtx, m.turnCancel = context.WithCancel(m.ctx)
	m.turnInterrupted = false
	m.turnCostStart = m.orch.SessionCost()
	m.activeAgentText.Reset()
	m.activeTurnItems = nil
	m.activityDetailBytes = 0
	m.activityIndex = map[string]int{"direct-subagent": 0}
	m.activityArgs = map[string]any{"direct-subagent": activitySummaryArgs(args)}
	m.pendingToolCalls = 1
	m.pendingSubagents = 1
	m.turnSubagents = 1
	m.appendTurnActivity(RenderToolActivity("", "subagent:"+name, args, false, nil))
	m.setLastToolMetadata("spawn_subagent", "direct-subagent", inspectionValue(args))
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
	root := m.workspaceRoot()
	agents := append([]string(nil), orch.ListAgents()...)
	// One byte per token is deliberately conservative. Spend at most a quarter
	// of the configured/live context window on the ENTIRE initial input; the
	// orchestrator still accounts for actual system/history/schema/output costs.
	limit := maxInputBytes
	if a := orch.ActiveAgent(); a != nil {
		window := a.ContextCfg.MaxContextTokens
		if a.Model != nil {
			if known, ok := model.KnownContextLimit(a.Model.Model()); ok && (window <= 0 || known < window) {
				window = known
			}
			if window <= 0 {
				window = model.ContextLimit(a.Model.Model(), 0)
			}
		}
		if window > 0 {
			limit = min(limit, window/4)
		}
	}
	return func() tea.Msg {
		input, err := prepareInput(ctx, root, message, agents, limit)
		if err != nil {
			return chatDoneMsg{turnID: turnID, attachments: input.Receipt, err: err}
		}
		if err := ctx.Err(); err != nil {
			return chatDoneMsg{turnID: turnID, attachments: input.Receipt, err: err}
		}
		if stream {
			result, err := StartExecution(ctx, orch, orchestrator.ExecutionRequest{
				Message:          input.Message,
				Mode:             orchestrator.ExecutionStreaming,
				SessionID:        orch.CurrentSessionID(),
				VerificationMode: orch.VerificationMode(),
			})
			if err != nil {
				return chatDoneMsg{turnID: turnID, contextReport: result.ContextReport, memoryIntent: result.MemoryIntent, attachments: input.Receipt, result: result, err: err}
			}
			return streamStartedMsg{turnID: turnID, ctx: ctx, ch: result.Stream, completion: result.Completion, execution: result, contextReport: result.ContextReport, memoryIntent: result.MemoryIntent, attachments: input.Receipt}
		}
		result, err := StartExecution(ctx, orch, orchestrator.ExecutionRequest{
			Message: input.Message, SessionID: orch.CurrentSessionID(), VerificationMode: orch.VerificationMode(),
		})
		return chatDoneMsg{turnID: turnID, resp: result.Response, contextReport: result.ContextReport, memoryIntent: result.MemoryIntent, attachments: input.Receipt, result: result, err: err}
	}
}

func (m *appModel) captureAttachmentReceipt(receipt string) {
	if receipt != "" {
		m.activeTurnItems = append(m.activeTurnItems, turnItem{kind: turnItemReceipt, content: strings.TrimSpace(receipt)})
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
		m.activeTurnItems[idx].result = RenderContextReport(report, intent, 0)
		return
	}
	m.appendActivity(activityContext, line)
	m.activeTurnItems[len(m.activeTurnItems)-1].result = RenderContextReport(report, intent, 0)
	if m.activityIndex == nil {
		m.activityIndex = make(map[string]int)
	}
	m.activityIndex["context-report"] = len(m.activeTurnItems) - 1
}

// StartExecution starts one TUI turn through the common execution boundary.
func StartExecution(ctx context.Context, orch *orchestrator.Orchestrator, request orchestrator.ExecutionRequest) (orchestrator.ExecutionResult, error) {
	return orch.Execute(ctx, request)
}

func (m *appModel) operationalSnapshotCmd(delay time.Duration) tea.Cmd {
	return func() tea.Msg {
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-m.ctx.Done():
				return nil
			}
		}
		return operationalSnapshotMsg{snapshot: m.orch.OperationalState(m.ctx)}
	}
}

func executionSnapshot(result orchestrator.ExecutionResult, running bool) orchestrator.ExecutionSnapshot {
	return orchestrator.ExecutionSnapshot{
		TaskID: result.TaskID, AgentID: result.AgentID, Running: running,
		StopReason: result.StopReason, Verification: result.Verification, Budget: result.Budget,
	}
}

func completionSnapshot(previous orchestrator.ExecutionSnapshot, completion orchestrator.ExecutionCompletion) orchestrator.ExecutionSnapshot {
	previous.Running = false
	previous.StopReason = completion.StopReason
	previous.Verification = completion.Verification
	previous.Budget = completion.Budget
	return previous
}

func resultError(result orchestrator.ExecutionResult) error {
	if result.TaskID == "" && result.StopReason == "" {
		return nil
	}
	return terminalExecutionError(result.StopReason, result.Verification.Allowed, result.Verification.Disagreement)
}

func completionError(completion orchestrator.ExecutionCompletion) error {
	if completion.Err != nil {
		return completion.Err
	}
	return terminalExecutionError(completion.StopReason, completion.Verification.Allowed, completion.Verification.Disagreement)
}

func terminalExecutionError(reason execution.StopReason, allowed, disagreement bool) error {
	if reason == "" && (!allowed || disagreement) {
		reason = execution.StopVerificationFailed
	}
	if reason == "" {
		return nil
	}
	if reason == execution.StopSuccess && allowed {
		return nil
	}
	if reason == execution.StopSuccess && !allowed {
		reason = execution.StopVerificationFailed
	}
	message := string(reason)
	if !allowed || disagreement {
		message = "verification does not support successful completion"
	}
	return &execution.TerminalError{Reason: reason, Retryable: controlEligible(controlResume, reason) || controlEligible(controlRetry, reason), Err: errors.New(message)}
}

type controlKind string

const (
	controlRetry  controlKind = "retry"
	controlResume controlKind = "resume"
)

type executionControl struct {
	Kind       controlKind
	TaskID     string
	StopReason execution.StopReason
}

func controlEligible(kind controlKind, reason execution.StopReason) bool {
	switch kind {
	case controlRetry:
		return reason == execution.StopProviderRetryable || reason == execution.StopTimeout
	case controlResume:
		return reason == execution.StopCancelled || reason == execution.StopBudgetExhausted || reason == execution.StopVerificationFailed || reason == execution.StopRepeatedFailure
	default:
		return false
	}
}

func (c executionControl) prompt() string {
	action := "Continue"
	if c.Kind == controlRetry {
		action = "Retry only the unfinished portion of"
	}
	return fmt.Sprintf("%s task %s after stop reason %s. Inspect the current session and workspace first. Do not repeat completed tool calls or committed side effects.", action, c.TaskID, c.StopReason)
}

func listenStream(ctx context.Context, turnID uint64, ch <-chan *model.ChatResponse, completion <-chan orchestrator.ExecutionCompletion) tea.Cmd {
	return func() tea.Msg {
		select {
		case resp, ok := <-ch:
			if !ok {
				return completedStreamMsg(ctx, turnID, completion)
			}
			return streamDeltaMsg{turnID: turnID, ctx: ctx, resp: resp, ch: ch, completion: completion}
		case <-ctx.Done():
			return streamDoneMsg{turnID: turnID, err: ctx.Err()}
		}
	}
}

func completedStreamMsg(ctx context.Context, turnID uint64, completion <-chan orchestrator.ExecutionCompletion) tea.Msg {
	if completion == nil {
		return streamDoneMsg{turnID: turnID, err: ctx.Err()}
	}
	value, ok := <-completion
	if !ok {
		return streamDoneMsg{turnID: turnID, err: fmt.Errorf("execution completion unavailable")}
	}
	return streamDoneMsg{turnID: turnID, completion: &value}
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
	if msg.turnID != m.turnID || !m.sending {
		return m, nil
	}
	resp := msg.resp
	if resp == nil {
		return m, listenStream(msg.ctx, msg.turnID, msg.ch, msg.completion)
	}
	if !resp.Delta {
		m.streamFinalReceived = true
		m.streamStopReason = resp.StopReason
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
				m.setLastToolMetadata(tc.Name, tc.ID, tc.Arguments)
				m.activityIndex[key] = len(m.activeTurnItems) - 1
				m.activityArgs[key] = activitySummaryArgs(args)
				m.pendingToolCalls++
				m.pendingSubagents++
				m.turnSubagents++
			}
		} else if m.activityCh == nil {
			m.appendTurnActivity(RenderToolCall(tc.Name, SummarizeArgs(tc.Arguments)))
			m.setLastToolMetadata(tc.Name, tc.ID, tc.Arguments)
			m.pendingToolCalls++
		}
	}
	if text := m.streamText(resp); text != "" {
		if m.activityCh == nil && m.pendingToolCalls > 0 && len(resp.ToolCalls) == 0 {
			label := progressLabel(m.pendingToolCalls, m.pendingSubagents, "completed")
			m.appendActivity(activityProgress, styleAgentName.Render("  ✓ "+label))
			m.pendingToolCalls = 0
			m.pendingSubagents = 0
		}
		m.appendTurnText(text)
	}
	if resp.Reasoning != "" {
		m.appendThinking(resp.Reasoning)
	}
	if resp.Err != nil {
		return m, m.finalizeTurn(resp.Err)
	}
	cmds := []tea.Cmd{listenStream(msg.ctx, msg.turnID, msg.ch, msg.completion)}
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

// Keep generation limits distinct from input-budget errors: compacting history
// does not complete an answer cut off by the provider's output allowance.
type incompleteResponseError struct{ message string }

func (e *incompleteResponseError) Error() string { return e.message }

func responseStopError(reason model.StopReason) error {
	switch reason {
	case model.StopReasonMaxTokens:
		return &incompleteResponseError{"response incomplete: model output token limit reached; ask to continue from where it stopped"}
	case model.StopReasonFilter:
		return &incompleteResponseError{"response incomplete: provider content filter stopped generation"}
	default:
		return nil
	}
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
			m.appendActivity(activityModel, line)
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
		m.activityArgs[activityKey] = activitySummaryArgs(data["args"])
		item := &m.activeTurnItems[m.activityIndex[activityKey]]
		item.toolName, item.agentID, item.callID = toolName, agentID, callID
		item.args = m.captureActivityValue(data["args"])
		item.started = time.Now()
	case chronosstream.EventToolResult:
		line := RenderToolActivity(label, toolName, m.activityArgs[activityKey], true, data["error"])
		if idx, ok := m.activityIndex[activityKey]; ok && idx < len(m.activeTurnItems) {
			m.activeTurnItems[idx].content = line
		} else {
			m.appendTurnActivity(line)
			m.activityIndex[activityKey] = len(m.activeTurnItems) - 1
		}
		item := &m.activeTurnItems[m.activityIndex[activityKey]]
		item.toolName, item.agentID, item.callID = toolName, agentID, callID
		item.result, item.failure = m.captureActivityValue(data["result"]), m.captureActivityValue(data["error"])
		if duration, ok := data["duration_ms"].(float64); ok {
			item.duration = time.Duration(duration * float64(time.Millisecond))
		} else if duration, ok := data["duration_ms"].(int64); ok {
			item.duration = time.Duration(duration) * time.Millisecond
		} else if duration, ok := data["duration_ms"].(int); ok {
			item.duration = time.Duration(duration) * time.Millisecond
		} else if duration, ok := data["duration"].(time.Duration); ok {
			item.duration = duration
		} else if !item.started.IsZero() {
			item.duration = time.Since(item.started)
			item.observedTime = true
		}
		if item.duration > 0 {
			prefix := ""
			if item.observedTime {
				prefix = "~"
			}
			item.content += styleDim.Render(" · " + prefix + item.duration.Round(time.Millisecond).String())
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
				m.appendActivity(activityRetry, line)
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
		if item.text == nil {
			item.text = &strings.Builder{}
			item.text.WriteString(item.content)
		}
		item.text.WriteString(text)
		item.content = item.text.String()
		item.rendered = ""
		return
	}
	b := &strings.Builder{}
	b.WriteString(text)
	m.activeTurnItems = append(m.activeTurnItems, turnItem{kind: turnItemText, content: b.String(), text: b})
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
		item.text.WriteString(text)
		item.content = item.text.String()
		item.rendered = ""
		return
	}
	b := &strings.Builder{}
	b.WriteString("thinking: " + text)
	m.appendActivity(activityThinking, b.String())
	m.activeTurnItems[len(m.activeTurnItems)-1].text = b
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
	m.appendActivity(activityTool, line)
}

func (m *appModel) appendActivity(kind activityKind, line string) {
	m.activeTurnItems = append(m.activeTurnItems, turnItem{kind: turnItemActivity, activity: kind, content: line})
}

func (m *appModel) setLastToolMetadata(name, id, args string) {
	item := &m.activeTurnItems[len(m.activeTurnItems)-1]
	item.toolName, item.callID, item.args = name, id, m.captureActivityValue(args)
}

func (m *appModel) captureActivityValue(value any) string {
	text := inspectionValue(value)
	if m.activityDetailBytes+len(text) > maxTranscriptBytes {
		return "[detail capture budget exhausted (4 MiB/turn); retrieve original source/artifact paths]"
	}
	m.activityDetailBytes += len(text)
	return text
}

func (m *appModel) setViewportContent(s string) {
	m.viewport.SetContentLines(lastNLines(s, maxViewportLines))
	m.viewportViewValid = false
}

func lastNLines(s string, n int) []string {
	if s == "" {
		return nil
	}
	if n <= 0 {
		return nil
	}
	cut := 0
	newlines := 0
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != '\n' {
			continue
		}
		newlines++
		if newlines == n {
			cut = i + 1
			break
		}
	}
	return strings.Split(s[cut:], "\n")
}

func joinLayout(parts ...string) string {
	var b strings.Builder
	size := 0
	for _, part := range parts {
		size += len(part) + 1
	}
	b.Grow(size)
	for i, part := range parts {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part)
	}
	return b.String()
}

func (m *appModel) refreshViewport() {
	m.setViewportContent(m.renderTranscript())
	if m.followOutput {
		m.viewport.GotoBottom()
		m.viewportViewValid = false
	}
}

func (m *appModel) afterViewportScroll() {
	wasFollowing := m.followOutput
	m.followOutput = m.viewport.AtBottom()
	m.viewportViewValid = false
	if !wasFollowing && m.followOutput {
		m.refreshViewport()
	}
}

func (m *appModel) handleShellEscape(cmdStr string) (tea.Model, tea.Cmd) {
	cmdStr = strings.TrimSpace(cmdStr)
	if cmdStr == "" {
		m.appendError(fmt.Errorf("usage: !<command>"))
		m.refreshViewport()
		return m, nil
	}
	m.history.Add("!" + cmdStr)
	m.appendSystem("$ " + cmdStr)
	m.statusMsg = "running shell"
	m.refreshViewport()
	orch := m.orch
	dir := m.workspaceRoot()
	ctx := m.ctx
	return m, func() tea.Msg {
		return runShellEscape(ctx, orch, dir, cmdStr)
	}
}

func runShellEscape(ctx context.Context, orch *orchestrator.Orchestrator, dir, cmdStr string) tea.Msg {
	if orch == nil {
		return shellDoneMsg{err: fmt.Errorf("shell: orchestrator is unavailable")}
	}
	result, err := orch.ExecuteTool(ctx, "shell", map[string]any{
		"command":     cmdStr,
		"working_dir": dir,
	})
	output, exitCode := shellToolOutput(result)
	if err == nil && exitCode != 0 {
		err = fmt.Errorf("exit status %d", exitCode)
	}
	if err != nil {
		err = fmt.Errorf("shell: %w", err)
	}
	return shellDoneMsg{output: output, err: err}
}

func shellToolOutput(result any) (string, int) {
	values, ok := result.(map[string]any)
	if !ok {
		if text, ok := result.(string); ok {
			return text, 0
		}
		return "", 0
	}
	stdout, _ := values["stdout"].(string)
	stderr, _ := values["stderr"].(string)
	exitCode, _ := values["exit_code"].(int)
	return stdout + stderr, exitCode
}

func truncateShellOutput(output string) string {
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return ""
	}
	truncated := false
	if len(output) > maxShellOutputBytes {
		output = output[len(output)-maxShellOutputBytes:]
		if i := strings.IndexByte(output, '\n'); i >= 0 {
			output = output[i+1:]
		}
		truncated = true
	}
	lines := strings.Split(output, "\n")
	if len(lines) > maxShellOutputLines {
		output = strings.Join(lines[len(lines)-maxShellOutputLines:], "\n")
		truncated = true
	}
	if truncated {
		return "… output truncated\n" + output
	}
	return output
}

func (m *appModel) workspaceRoot() string {
	if m.orch != nil {
		if ws := m.orch.Workspace(); ws != nil && ws.Root != "" {
			return ws.Root
		}
	}
	return m.workDir
}

// maintenanceCmd establishes cancellation/turn identity on Update, but invokes
// the operation only when Bubble Tea runs the command. Synchronous approval
// callbacks can therefore wait for Update without deadlocking it.
func (m *appModel) maintenanceCmd(status string, run func(context.Context) (string, error)) tea.Cmd {
	m.sending = true
	m.turnID++
	m.turnCtx, m.turnCancel = context.WithCancel(m.ctx)
	m.turnInterrupted = false
	m.statusMsg = status
	ctx, turnID := m.turnCtx, m.turnID
	return func() tea.Msg {
		if err := ctx.Err(); err != nil {
			return maintenanceDoneMsg{turnID: turnID, err: err}
		}
		text, err := run(ctx)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return maintenanceDoneMsg{turnID: turnID, text: text, err: err}
	}
}

func (m *appModel) handleMCPCommand(arg string) tea.Cmd {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		m.appendSystem(m.mcpStatusText())
		return nil
	}
	switch fields[0] {
	case "connect":
		if len(fields) != 2 {
			m.appendError(fmt.Errorf("usage: /mcp connect <name>"))
			return nil
		}
		if m.sending {
			m.appendError(fmt.Errorf("cannot connect MCP while a response is in progress"))
			return nil
		}
		orch, name := m.orch, fields[1]
		return m.maintenanceCmd("connecting MCP", func(ctx context.Context) (string, error) {
			status, err := orch.ConnectMCP(ctx, name)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("connected %s (%d tools)", status.Name, status.Tools), nil
		})
	default:
		m.appendError(fmt.Errorf("unknown mcp command %q (try /mcp or /mcp connect <name>)", fields[0]))
	}
	return nil
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
			m.invalidateAuthIdentity()
			m.refreshPrompt()
			m.appendSystem(fmt.Sprintf("logged out of %q", arg))
		}
	case "/whoami":
		m.handleWhoamiCommand(arg)
	case "/context":
		m.handleContextCommand()
	case "/inspect":
		if arg == "operational" {
			m.openInspection("Operational state · read-only", RenderOperationalSnapshot(m.operational, 0))
			return m, nil
		}
		if arg != "" && arg != "context" && arg != "changes" {
			m.appendError(fmt.Errorf("usage: /inspect [context|changes|operational]"))
			break
		}
		m.inspectTurn(arg)
		return m, nil
	case "/usage":
		m.appendSystem(m.usageSummary())
	case "/status":
		m.appendSystem(RenderOperationalSnapshot(m.operational, m.viewport.Width()))
	case "/task":
		kind := controlKind(strings.ToLower(arg))
		if kind != controlRetry && kind != controlResume {
			m.appendError(fmt.Errorf("usage: /task retry|resume"))
			break
		}
		control := executionControl{Kind: kind, TaskID: m.lastExecution.TaskID, StopReason: m.lastExecution.StopReason}
		if control.TaskID == "" || !controlEligible(kind, control.StopReason) {
			m.appendError(fmt.Errorf("task %s is not eligible after stop reason %q", kind, control.StopReason))
			break
		}
		m.appendSystem(fmt.Sprintf("%s task %s without replaying its original request", kind, control.TaskID))
		return m.handleSubmit(control.prompt())
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
		m.blockSources = nil
		m.rawBlockBytes = 0
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
		m.lastExecution = orchestrator.ExecutionSnapshot{}
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
		if arg == "list" {
			return m, m.openSessionPicker()
		}
		m.appendSystem("current session: " + m.orch.CurrentSessionID() + "\n/session list opens searchable sessions; /resume continues the latest.")
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
		cmd := m.handleMCPCommand(arg)
		m.refreshViewport()
		return m, cmd
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
		orch := m.orch
		return m, m.maintenanceCmd("compacting session", func(ctx context.Context) (string, error) {
			if err := orch.CompactActiveSession(ctx); err != nil {
				return "", err
			}
			return "session compacted", nil
		})
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
			m.statusMsg = "mouse scrolling on · shift+drag or ctrl+shift+c to copy"
		} else {
			m.statusMsg = "mouse scrolling off · drag selects text · pgup/pgdown still scroll"
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
		all := strings.TrimRight(ansi.Strip(m.copyTranscriptText()), "\n")
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
	if m.inspection != nil && m.approval == nil {
		return strings.TrimRight(ansi.Strip(m.inspection.viewport.View()), "\n")
	}
	return strings.TrimRight(ansi.Strip(m.viewport.View()), "\n")
}

// Full retained source is assembled only for an explicit clipboard request,
// never on the render path. Fences and long lines survive viewport clipping.
func (m *appModel) copyTranscriptText() string {
	var b strings.Builder
	writeItems := func(items []turnItem) {
		for _, item := range items {
			b.WriteString(item.content)
			b.WriteString("\n\n")
		}
	}
	for i, block := range m.blocks {
		if i < len(m.blockSources) && m.blockSources[i] != nil {
			source := m.blockSources[i]
			if source.user != "" {
				b.WriteString("❯ you\n" + source.user)
			} else {
				b.WriteString("✦ " + source.name + "\n")
				writeItems(source.items)
				if source.err != nil {
					b.WriteString(classifyErrorMessage(source.err))
				}
			}
		} else if m.hasLastTurn && i == m.lastTurnBlockIdx {
			writeItems(m.lastTurnItems)
		} else {
			b.WriteString(block)
		}
		b.WriteString("\n\n")
	}
	if m.sending {
		writeItems(m.activeTurnItems)
	}
	return b.String()
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
	m.invalidateAuthIdentity()
	m.refreshPrompt()
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
		m.invalidateAuthIdentity()
		m.refreshPrompt()
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
	if contextTokens := m.lastKnownUsage.WindowTokens(); contextTokens > 0 {
		fmt.Fprintf(&b, "latest model context: %s tokens\n", formatTokenCount(contextTokens))
	}
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
	contextTokens := int64(m.lastKnownUsage.WindowTokens())
	if contextTokens == 0 {
		contextTokens = input + cacheRead + cacheWrite + output
	}
	return fmt.Sprintf("last turn: input %d │ cache read %d │ cache write %d │ output %d │ context %d │ cost %s\nexecution: %d model calls │ %d subagents\nsession: input %d │ cache read %d │ output %d │ cost %s",
		input, cacheRead, cacheWrite, output, contextTokens, m.formatCost(m.lastTurnCost.SpentMicrodollars), m.lastModelCalls, m.lastSubagents,
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

// pendingSubagentSegment reports the model of the subagent currently running
// via spawn_subagent, if any. It scans m.activityArgs rather than tracking a
// dedicated field because spawn_subagent's "agent" argument is already
// captured there under whichever key handleModelResp/handleActivity used for
// that tool call.
func (m *appModel) pendingSubagentSegment() string {
	if m.pendingSubagents == 0 || m.orch == nil {
		return ""
	}
	for _, v := range m.activityArgs {
		args, ok := v.(map[string]any)
		if !ok {
			continue
		}
		name, _ := args["agent"].(string)
		if name == "" {
			continue
		}
		if _, modelID, ok := m.orch.AgentModelInfo(name); ok && modelID != "" {
			return "sub:" + name + "@" + modelID
		}
		return "sub:" + name
	}
	return "sub:dynamic"
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
	switch {
	case m.signedIn():
		m.input.Placeholder = "Message chronos-code..."
	case m.width < 30:
		// Full and mid-length placeholders below wrap onto a second input
		// line at these widths, growing bottomView past its normal 3-line
		// budget (border + 1 content line + border) and overflowing the
		// fixed-height layout in very narrow terminals.
		m.input.Placeholder = "Sign in"
	case m.width < 72:
		m.input.Placeholder = "Not signed in — /login"
	default:
		m.input.Placeholder = "Not signed in — /login or Ctrl+L · Claude Code, Codex, or API key"
	}
	if m.width > 0 {
		m.input.SetWidth(m.width - inputBoxBorderWidth - inputBoxPaddingWidth)
	}
}

func (m *appModel) invalidateAuthIdentity() {
	m.authCheckedAt = time.Time{}
	m.authCatalogAt = time.Time{}
	m.authCatalog = nil
}

func (m *appModel) ensureAuthIdentity() {
	if m.orch == nil {
		m.authSignedIn = false
		m.authModelID = ""
		return
	}
	if !m.authCheckedAt.IsZero() && time.Since(m.authCheckedAt) < authIdentityTTL {
		return
	}
	candidates := []string{"anthropic", "openai"}
	provider, modelID := m.orch.ActiveModelInfo()
	m.authModelID = modelID
	if provider != "" && provider != "anthropic" && provider != "openai" {
		candidates = append(candidates, provider)
	}
	m.authSignedIn = len(m.orch.AuthorizedProviders(m.ctx, candidates)) > 0
	m.authCheckedAt = time.Now()
}

func (m *appModel) authorizedProviderNames() []string {
	if m.orch == nil {
		return nil
	}
	if !m.authCatalogAt.IsZero() && time.Since(m.authCatalogAt) < authIdentityTTL {
		return m.authCatalog
	}
	m.authCatalog = m.orch.AuthorizedProviders(m.ctx, distinctProviders(modelinfo.All()))
	m.authCatalogAt = time.Now()
	return m.authCatalog
}

func (m *appModel) signedIn() bool {
	m.ensureAuthIdentity()
	return m.authSignedIn
}

func (m *appModel) sessionIdentitySegment() string {
	if m.orch == nil {
		return ""
	}
	m.ensureAuthIdentity()
	if !m.authSignedIn {
		return "not signed in · /login"
	}
	if m.authModelID == "" {
		return "signed in"
	}
	return m.authModelID
}

// appendUserTurn, appendSystem and appendError all wrap to m.viewport.Width():
// the viewport itself never wraps long lines, so an unwrapped line can
// overflow into and visually corrupt the fixed-height chrome below it — the
// same class of bug that made the status bar wrap onto a second line (see
// styleHeaderBar's comment in styles.go).
func (m *appModel) appendUserTurn(line string) {
	header := RenderTurnHeader("❯", "you", styleUserPrefix, m.viewport.Width())
	body := wrapText(boundedTextTail(line, maxItemRenderBytes, maxViewportLines), m.viewport.Width())
	m.appendBlock(header + "\n" + body)
	m.setBlockSource(&transcriptSource{user: line, bytes: len(line), width: m.viewport.Width()})
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
	var incomplete *incompleteResponseError
	if errors.As(err, &incomplete) {
		return incomplete.Error()
	}
	var classified *apierror.Classified
	if !errors.As(err, &classified) {
		classified = apierror.Classify(err)
	}
	if classified != nil {
		if classified.Category == apierror.CategoryContextLength || classified.Category == apierror.CategoryRequestTooLarge {
			return "Request exceeds the model input budget. Use /compact to summarize history, or reduce attachments. /clear starts a fresh conversation. Completed actions were not replayed."
		}
		return classified.Message
	}
	return "error: " + err.Error()
}

// classifyStatusMessage returns a short status bar label for a failed request.
func classifyStatusMessage(err error) string {
	var incomplete *incompleteResponseError
	if errors.As(err, &incomplete) {
		return "response incomplete"
	}
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
	m.blockSources = append(m.blockSources, nil)
	m.blockBytes += len(block)
	m.finalizedDirty = true
	m.trimTranscript()
}

func (m *appModel) setBlockSource(source *transcriptSource) {
	if len(m.blockSources) == 0 || source.bytes > maxTranscriptBytes {
		return
	}
	m.blockSources[len(m.blockSources)-1] = source
	m.rawBlockBytes += source.bytes
	for i := 0; m.rawBlockBytes > maxTranscriptBytes && i < len(m.blockSources); i++ {
		if old := m.blockSources[i]; old != nil {
			m.rawBlockBytes -= old.bytes
			m.blockSources[i] = nil
		}
	}
}

func (m *appModel) trimTranscript() {
	trimmed := 0
	for (m.blockBytes > maxTranscriptBytes || len(m.blocks) > maxViewportLines) && len(m.blocks) > 1 {
		m.blockBytes -= len(m.blocks[0])
		m.blocks[0] = ""
		m.blocks = m.blocks[1:]
		if len(m.blockSources) > 0 {
			if source := m.blockSources[0]; source != nil {
				m.rawBlockBytes -= source.bytes
			}
			m.blockSources[0] = nil
			m.blockSources = m.blockSources[1:]
		}
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
	// Never replay activeRequest: earlier tool calls may already have mutated
	// the workspace. Recoverable model-call retries belong to the SDK.
	m.settleTurnActivities(err)
	m.sending = false
	for i := range m.activeTurnItems {
		m.activeTurnItems[i].settled = true
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
	items := cloneTurnItems(m.activeTurnItems)
	m.lastTurnErr = err
	m.lastTurnInterrupted = interrupted
	m.appendBlock(m.buildAssistantBlock(items, interrupted, err))
	m.lastTurnItems = items
	source := &transcriptSource{items: cloneTurnItems(m.lastTurnItems), name: m.displayAgentName(),
		interrupted: interrupted, err: err, width: m.viewport.Width()}
	for _, item := range source.items {
		source.bytes += len(item.content) + len(item.args) + len(item.result) + len(item.failure)
	}
	m.setBlockSource(source)
	m.hasLastTurn = true
	m.lastTurnBlockIdx = len(m.blocks) - 1
	if m.activeAgentText.Len() > 0 {
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
			m.statusMsg = "budget exhausted │ /compact to continue"
		} else {
			m.statusMsg = classifyStatusMessage(err) + " · " + m.usageStatus()
		}
	} else {
		m.statusMsg = m.usageStatus()
	}
	m.turnInterrupted = false
	m.activeAgentText.Reset()
	m.activeTurnItems = nil
	m.activityDetailBytes = 0
	m.activityIndex = nil
	m.activityArgs = nil
	m.pendingToolCalls = 0
	m.pendingSubagents = 0
	m.lastChunk = ""
	m.activeRequest = ""
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

func (m *appModel) renderTranscript() string {
	finalized := m.renderFinalizedTranscript()
	if !m.sending {
		return finalized
	}
	body := styleDim.Render(m.spin.View() + " thinking...")
	if len(m.activeTurnItems) > 0 {
		body = m.renderTurnItems()
	}
	active := RenderTurnHeader("✦", m.displayAgentName(), styleAgentName, m.viewport.Width()) + "\n" + body
	return boundedTranscriptJoin([]string{finalized, active})
}

func (m *appModel) renderFinalizedTranscript() string {
	if !m.finalizedDirty && m.finalizedCount == len(m.blocks) {
		return m.finalizedText
	}
	var parts []string
	remaining, lines := maxRenderBytes, 0
	for i := len(m.blocks) - 1; i >= 0 && remaining > 0 && lines < maxViewportLines; i-- {
		block := m.blocks[i]
		if i < len(m.blockSources) {
			if source := m.blockSources[i]; source != nil && source.width != m.viewport.Width() {
				if source.user != "" {
					block = RenderTurnHeader("❯", "you", styleUserPrefix, m.viewport.Width()) + "\n" +
						wrapText(boundedTextTail(source.user, maxItemRenderBytes, maxViewportLines), m.viewport.Width())
				} else {
					block = m.buildAssistantBlockNamed(source.items, source.interrupted, source.err, source.name)
				}
				m.blockBytes += len(block) - len(m.blocks[i])
				m.blocks[i] = block
				source.width = m.viewport.Width()
			}
		}
		block = boundedTextTail(block, remaining, maxViewportLines-lines)
		parts = append(parts, block)
		remaining -= len(block) + 2
		lines += strings.Count(block, "\n") + 2
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	m.trimTranscript()
	if m.trimmedBlocks > 0 && remaining > 0 && lines < maxViewportLines {
		parts = append([]string{styleDim.Render(fmt.Sprintf("[%d older transcript blocks omitted]", m.trimmedBlocks))}, parts...)
	}
	m.finalizedText = boundedTranscriptJoin(parts)
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
	for i := range out {
		out[i].text = nil // Snapshot strings remain immutable as a live builder grows.
	}
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
	return m.buildAssistantBlockNamed(items, interrupted, err, m.displayAgentName())
}

func (m *appModel) buildAssistantBlockNamed(items []turnItem, interrupted bool, err error, name string) string {
	var b strings.Builder
	b.WriteString(RenderTurnHeader("✦", name, styleAgentName, m.viewport.Width()))
	if err != nil {
		b.WriteByte('\n')
		if len(items) > 0 {
			b.WriteString(m.renderItemList(items))
			b.WriteString("\n\n")
		}
		message := classifyErrorMessage(err)
		if strings.Contains(err.Error(), "token budget exceeded for session") {
			message += "\n\nThis session has reached its cumulative token limit. Use /compact to summarize history and reset its budget. Use /clear to start a fresh session (active conversation context is discarded). Completed actions were not replayed."
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
	start, size := len(items), 0
	for start > 0 && len(items)-start < maxViewportLines {
		n := min(len(items[start-1].content), maxItemRenderBytes) + 2
		if size+n > maxRenderBytes && start < len(items) {
			break
		}
		size += n
		start--
	}
	items = items[start:]
	var rendered string
	if m.toolsExpanded {
		rendered = m.renderExpandedItems(items)
	} else {
		rendered = m.renderCollapsedItems(items)
	}
	return boundedTextTail(rendered, maxRenderBytes, maxViewportLines)
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
		if items[i].kind != turnItemActivity || items[i].activity != activityTool {
			if wrote {
				b.WriteString("\n\n")
			}
			b.WriteString(m.renderOneItem(&items[i]))
			wrote = true
			i++
			continue
		}
		j := i
		for j < len(items) && items[j].kind == turnItemActivity && items[j].activity == activityTool {
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
	content := boundedTextTail(item.content, maxItemRenderBytes, maxViewportLines)
	if item.kind == turnItemReceipt {
		return styleDim.Render(wrapText(content, m.viewport.Width()))
	}
	if item.kind == turnItemActivity {
		return truncateToWidth(content, m.viewport.Width())
	}
	width := m.viewport.Width()
	if item.rendered != "" && item.renderedWidth == width {
		return item.rendered
	}
	if m.sending && !item.settled {
		item.rendered = wrapText(content, width)
	} else {
		item.rendered = RenderMarkdownLite(content, width)
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

	parts := []string{m.renderHeaderBar(), m.transcriptView(), m.bottomView}
	if operational := m.renderOperationalBar(); operational != "" {
		parts = append(parts, operational)
	}
	parts = append(parts, m.renderStatusBar())
	view := tea.View{
		Content:   joinLayout(parts...),
		AltScreen: true,
	}
	if m.inspection != nil && m.approval == nil {
		parts = []string{m.renderHeaderBar(), m.inspection.View()}
		if operational := m.renderOperationalBar(); operational != "" {
			parts = append(parts, operational)
		}
		parts = append(parts, m.renderStatusBar())
		view.Content = joinLayout(parts...)
	}
	if m.mouseCapture {
		view.MouseMode = tea.MouseModeCellMotion
	}
	return view
}

func (m *appModel) renderOperationalBar() string {
	if m.width <= 0 || (m.height > 0 && m.height < 16) || (m.picker == nil && m.wizard == nil && m.approval == nil && !m.searching && len(m.inputCompletions()) > 0) {
		return ""
	}
	snapshot := m.operational
	if snapshot.Execution.TaskID == "" && m.lastExecution.TaskID != "" {
		snapshot.Execution = m.lastExecution
	}
	text := fmt.Sprintf(" safety:%s · plan-only:%t · verify:%s/%s",
		emptyLabel(snapshot.PermissionMode), snapshot.PlanOnly, emptyLabel(string(snapshot.VerificationMode)), operationalVerification(snapshot.Execution))
	overlay := m.picker != nil || m.wizard != nil || m.approval != nil || m.searching || m.inspection != nil
	if specialists := m.activeSpecialists(snapshot.ActiveSpecialists); !overlay && len(specialists) > 0 {
		text = appendOperationalSegment(text, "specialists:"+strings.Join(boundedStatusValues(specialists, 3), ","), m.width)
	}
	if !overlay && len(snapshot.WorktreeIDs) > 0 {
		ids := make([]string, len(snapshot.WorktreeIDs))
		for i, id := range snapshot.WorktreeIDs {
			ids[i] = shortOperationalID(id)
		}
		text = appendOperationalSegment(text, "worktrees:"+strings.Join(boundedStatusValues(ids, 3), ","), m.width)
	}
	text = appendOperationalSegment(text, "limits:"+renderRemainingLimits(snapshot.Execution), m.width)
	text = appendOperationalSegment(text, "plan:"+renderPlanState(snapshot.Plan), m.width)
	return styleDim.Render(truncateToWidth(text, m.width))
}

func appendOperationalSegment(text, segment string, width int) string {
	candidate := text + " · " + segment
	if lipgloss.Width(candidate) <= width {
		return candidate
	}
	return text
}

func boundedStatusValues(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	result := append([]string(nil), values[:limit]...)
	return append(result, fmt.Sprintf("+%d", len(values)-limit))
}

func shortOperationalID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

func (m *appModel) activeSpecialists(snapshot []orchestrator.SpecialistSnapshot) []string {
	unique := make(map[string]struct{})
	result := make([]string, 0, len(snapshot))
	add := func(name string) {
		if name == "" {
			return
		}
		name = "@" + name
		if _, exists := unique[name]; exists {
			return
		}
		unique[name] = struct{}{}
		result = append(result, name)
	}
	for _, specialist := range snapshot {
		add(specialist.AgentID)
	}
	if m.pendingSubagents > 0 {
		var pending []string
		for _, value := range m.activityArgs {
			args, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if name, _ := args["agent"].(string); name != "" {
				pending = append(pending, name)
			}
		}
		sort.Strings(pending)
		for _, name := range pending {
			add(name)
		}
	}
	if len(result) > 8 {
		result = result[:8]
	}
	return result
}

func operationalVerification(snapshot orchestrator.ExecutionSnapshot) string {
	if snapshot.TaskID == "" {
		return "idle"
	}
	if snapshot.Running {
		return "pending"
	}
	if snapshot.Verification.Disagreement {
		return "failed"
	}
	if !snapshot.Verification.Allowed {
		return "blocked"
	}
	return "satisfied"
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
	// highlightRanges track [start,end) grapheme offsets (matching
	// lipgloss.StyleRanges/ansi.Cut, not byte offsets) of segments below that
	// get their own color on top of styleStatusLeft's base styling, applied
	// after truncation/rendering so the surrounding background survives.
	var highlightRanges []lipgloss.Range
	highlight := func(text *string, style lipgloss.Style, appendSeg func() string) {
		start := utf8.RuneCountInString(*text)
		*text += appendSeg()
		highlightRanges = append(highlightRanges, lipgloss.NewRange(start, utf8.RuneCountInString(*text), style))
	}

	leftText := " ● " + runLabel + " │ " + streamLabel
	if m.orch.ActiveID() != m.orch.PrimaryID() {
		leftText = " ● " + runLabel + " │ @" + m.orch.ActiveID() + " │ " + streamLabel
	}
	// Keep the resume hint ahead of model/routing metadata so it cannot be
	// truncated off-screen while output is paused near the bottom.
	if !m.followOutput {
		leftText += fmt.Sprintf(" │ scrolled %d%% · ctrl+end follow", int(m.viewport.ScrollPercent()*100))
	}
	if m.orch.PlanMode() {
		leftText += " │ plan"
	}
	if _, modelID := m.orch.ActiveModelInfo(); modelID != "" {
		leftText += " │ "
		highlight(&leftText, styleStatusModel.Style, func() string { return modelID })
	}
	if m.width >= 100 {
		if think := m.orch.ThinkingLevel(); think != "off" {
			leftText += " │ think:" + think
		}
		leftText += " │ " + string(m.orch.VerificationMode())
		if route := m.orch.LastRouteStatus(); route != "route:—" {
			leftText += " │ " + route
		}
		if seg := m.pendingSubagentSegment(); seg != "" {
			leftText += " │ "
			highlight(&leftText, styleStatusSub.Style, func() string { return seg })
		}
		if label, age, ok := m.orch.LastHookActivity(); ok && age < 30*time.Second {
			leftText += " │ "
			style := styleStatusHookOK.Style
			if strings.Contains(label, "✗") {
				style = styleStatusHookFail.Style
			}
			highlight(&leftText, style, func() string { return "hook:" + label })
		}
	}
	if ident := m.sessionIdentitySegment(); ident != "" {
		leftText += " │ " + ident
	}
	if ctxSeg := m.contextUsageSegment(); ctxSeg != "" {
		leftText += " │ " + ctxSeg
	}
	if len(m.queuedMessages) > 0 {
		leftText += fmt.Sprintf(" │ queued %d", len(m.queuedMessages))
	}
	leftText += " "
	if m.width < 72 {
		leftText = " ● " + runLabel
		if m.orch.PlanMode() {
			leftText += " │ plan"
		}
		if len(m.queuedMessages) > 0 {
			leftText += fmt.Sprintf(" +%d", len(m.queuedMessages))
		}
		if !m.followOutput {
			leftText += " ↑ ctrl+end"
		}
		leftText += " "
		highlightRanges = nil
	}
	leftText = truncateToWidth(leftText, m.width)
	leftSeg := styleStatusLeft.Render(leftText)
	if len(highlightRanges) > 0 {
		// Render highlighted spans as their own complete lipgloss chunks
		// (each carrying styleStatusLeft's Background/Bold plus its own
		// Foreground) rather than post-processing leftSeg with
		// lipgloss.StyleRanges: StyleRanges' "existing styles taken into
		// account" only preserves ANSI that was already distributed through
		// the input string, but styleStatusLeft.Render(leftText) applies a
		// single open/reset pair around the whole line, so text after a
		// range's own reset would otherwise render with no background.
		runes := []rune(leftText)
		plainLen := len(runes)
		var b strings.Builder
		pos := 0
		for _, r := range highlightRanges {
			if r.Start >= plainLen || r.Start < pos {
				continue
			}
			end := r.End
			if end > plainLen {
				end = plainLen
			}
			b.WriteString(styleStatusLeft.Render(string(runes[pos:r.Start])))
			// r.Style is the raw lipgloss.Style stashed in the range (see
			// highlight() below); route it through lipgloss.Sprint exactly
			// like terminalStyle.Render does, otherwise this segment skips
			// the output-profile filtering the rest of the bar gets and can
			// emit truecolor codes on terminals/pipes that don't support them.
			b.WriteString(lipgloss.Sprint(r.Style.Render(string(runes[r.Start:end]))))
			pos = end
		}
		b.WriteString(styleStatusLeft.Render(string(runes[pos:])))
		leftSeg = b.String()
	}

	rightText := " drag-select copy │ ctrl+shift+c last │ ctrl+/ commands │ ctrl+c interrupt/quit "
	if m.mouseCapture {
		rightText = " wheel scroll │ shift+drag copy │ ctrl+shift+c last │ ctrl+/ commands "
	}
	// Errors can contain provider newlines; fixed chrome must remain one row.
	status := strings.Join(strings.Fields(m.statusMsg), " ")
	if status != "" {
		rightText = " " + status + " │" + rightText
	}
	if m.width < 90 {
		rightText = " " + status + " "
	}
	rightSeg := styleStatusRight.Render(rightText)
	if lipgloss.Width(leftSeg)+lipgloss.Width(rightSeg) > m.width {
		rightText = " " + status + " "
		available := m.width - lipgloss.Width(leftSeg)
		if available <= 0 {
			rightSeg = ""
		} else {
			rightSeg = styleStatusRight.Render(truncateToWidth(rightText, available))
		}
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
