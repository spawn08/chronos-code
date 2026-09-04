package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spawn08/chronos/engine/model"
	chronosstream "github.com/spawn08/chronos/engine/stream"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/orchestrator"
)

type approvalInstallerStub struct {
	handler tool.ApprovalFunc
	calls   int
}

func (s *approvalInstallerStub) SetApprovalHandler(handler tool.ApprovalFunc) {
	s.handler = handler
	s.calls++
}

func newTestAppModel(t *testing.T) *appModel {
	t.Helper()
	root := t.TempDir()
	indexOnStart := false
	cfg := &config.Config{
		FileConfig: agent.FileConfig{
			Defaults: &agent.AgentConfig{Storage: agent.StorageConfig{
				Backend: "sqlite",
				DSN:     filepath.Join(root, "sessions.db"),
			}},
			Agents: []agent.AgentConfig{{
				ID:   "coder",
				Name: "Coder",
				Model: agent.ModelConfig{
					Provider: "openai",
					Model:    "gpt-4o-mini",
					APIKey:   "test-key",
				},
			}},
		},
		Workspace: config.WorkspaceConfig{Root: root, IndexOnStart: &indexOnStart},
	}
	orch, err := orchestrator.New(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("orchestrator.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := orch.Close(); err != nil {
			t.Errorf("orchestrator.Close() error = %v", err)
		}
	})

	ta := newComposer()
	return &appModel{
		orch:           orch,
		ctx:            context.Background(),
		cancel:         func() {},
		input:          ta,
		history:        NewHistory(),
		clipboardRead:  func() (string, error) { return "", fmt.Errorf("unexpected clipboard read") },
		clipboardWrite: func(string) error { return fmt.Errorf("unexpected clipboard write") },
	}
}

func TestFrameTiming_Stats_Empty(t *testing.T) {
	var ft frameTiming
	got := ft.stats()
	if !strings.Contains(got, "no frame timing data") {
		t.Errorf("empty frameTiming.stats() = %q, want 'no frame timing data' message", got)
	}
}

func TestFrameTiming_Stats_WithSamples(t *testing.T) {
	var ft frameTiming
	for i := 0; i < 10; i++ {
		ft.samples[i] = time.Duration(i+1) * time.Millisecond
		ft.sampleIdx = (i + 1) % frameTimingSamples
		ft.sampleCount = i + 1
	}
	got := ft.stats()
	if !strings.Contains(got, "10 samples") {
		t.Errorf("frameTiming.stats() = %q, want '10 samples'", got)
	}
	if !strings.Contains(got, "p50=") || !strings.Contains(got, "p95=") || !strings.Contains(got, "p99=") {
		t.Errorf("frameTiming.stats() = %q, want p50/p95/p99 labels", got)
	}
}

func TestFrameTiming_RecordCycle(t *testing.T) {
	var ft frameTiming
	ft.recordUpdateStart()
	time.Sleep(time.Microsecond)
	ft.recordViewEnd()
	if ft.sampleCount != 1 {
		t.Fatalf("sampleCount = %d, want 1", ft.sampleCount)
	}
	if ft.samples[0] <= 0 {
		t.Errorf("samples[0] = %v, want > 0", ft.samples[0])
	}
}

func TestFrameTiming_RecordViewEnd_NoStart(t *testing.T) {
	var ft frameTiming
	ft.recordViewEnd()
	if ft.sampleCount != 0 {
		t.Errorf("sampleCount = %d after recordViewEnd with no start, want 0", ft.sampleCount)
	}
}

func TestFrameTiming_Wraps(t *testing.T) {
	var ft frameTiming
	for i := 0; i < frameTimingSamples+20; i++ {
		ft.recordUpdateStart()
		ft.recordViewEnd()
	}
	if ft.sampleCount != frameTimingSamples {
		t.Errorf("sampleCount = %d after %d cycles, want %d", ft.sampleCount, frameTimingSamples+20, frameTimingSamples)
	}
}

func TestRenderTranscript_CachesBlocks(t *testing.T) {
	m := &appModel{}
	m.appendBlock("block-A")
	m.appendBlock("block-B")

	first := m.renderTranscript()
	if m.finalizedDirty || m.finalizedText == "" {
		t.Fatal("renderTranscript() did not cache finalized transcript")
	}

	m.appendBlock("block-C")
	got := m.renderTranscript()
	if got == first || !strings.Contains(got, "block-A") || !strings.Contains(got, "block-C") {
		t.Errorf("renderTranscript() = %q, want all blocks present", got)
	}
}

func TestRenderTurnItems_CachesMarkdownByWidth(t *testing.T) {
	m := &appModel{activeTurnItems: []turnItem{{kind: turnItemText, content: "hello"}}}
	_ = m.renderTurnItems()
	if m.activeTurnItems[0].rendered != "hello" {
		t.Fatalf("rendered text = %q, want hello", m.activeTurnItems[0].rendered)
	}
	m.invalidateRenderCache()
	if m.activeTurnItems[0].rendered != "" {
		t.Fatal("width invalidation retained active markdown cache")
	}
}

func TestInvalidateRenderCache(t *testing.T) {
	m := &appModel{}
	m.appendBlock("a")
	m.appendBlock("b")
	_ = m.renderTranscript()
	if m.finalizedText == "" {
		t.Fatal("finalized transcript was not cached")
	}
	m.invalidateRenderCache()
	if m.finalizedText != "" || !m.finalizedDirty {
		t.Errorf("finalized cache was not invalidated: text=%q dirty=%v", m.finalizedText, m.finalizedDirty)
	}
}

func TestAppendBlockBoundsTranscriptMemory(t *testing.T) {
	m := &appModel{}
	m.appendBlock(strings.Repeat("x", maxTranscriptBytes))
	m.appendBlock("latest")

	if len(m.blocks) != 1 || m.blocks[0] != "latest" {
		t.Fatalf("bounded blocks = %d, %q; want only latest", len(m.blocks), m.blocks[0])
	}
	if m.blockBytes != len("latest") || m.trimmedBlocks != 1 {
		t.Fatalf("block accounting = %d bytes, %d trimmed", m.blockBytes, m.trimmedBlocks)
	}
	if got := m.renderTranscript(); !strings.Contains(got, "older transcript blocks omitted") {
		t.Errorf("trimmed transcript has no omission marker: %q", got)
	}
}

// TestHandleKey_AltEnterQueuesWhileSending covers AC-2: Alt+Enter while a
// turn is streaming captures the input into queuedMessages instead of
// inserting a newline.
func TestHandleKey_AltEnterQueuesWhileSending(t *testing.T) {
	ta := textarea.New()
	ta.SetValue("follow-up message")
	m := &appModel{input: ta, sending: true}

	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})

	if got := m.queuedMessages; len(got) != 1 || got[0] != "follow-up message" {
		t.Errorf("queuedMessages = %q, want one follow-up", got)
	}
	if m.input.Value() != "" {
		t.Errorf("input.Value() = %q after queuing, want empty", m.input.Value())
	}
}

// TestHandleKey_AltEnterDoesNotQueueWhenIdle covers the negative case: with
// no turn streaming, Alt+Enter must not touch queuedMessages (it falls
// through to the textarea's own insert-newline binding instead).
func TestHandleKey_AltEnterDoesNotQueueWhenIdle(t *testing.T) {
	ta := textarea.New()
	ta.SetValue("still typing")
	m := &appModel{input: ta, sending: false}

	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})

	if len(m.queuedMessages) != 0 {
		t.Errorf("queuedMessages = %q, want empty when not sending", m.queuedMessages)
	}
}

func TestHandleKey_V2PickerShortcuts(t *testing.T) {
	tests := []struct {
		name        string
		msg         tea.KeyPressMsg
		wantHeading string
	}{
		{name: "Ctrl+A", msg: tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl}, wantHeading: "Switch agent:"},
		{name: "Ctrl+M", msg: tea.KeyPressMsg{Code: 'm', Mod: tea.ModCtrl}, wantHeading: "Switch model"},
		{name: "Ctrl+/", msg: tea.KeyPressMsg{Code: '/', Mod: tea.ModCtrl}, wantHeading: "Commands:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestAppModel(t)
			_, cmd := m.Update(tt.msg)
			if cmd != nil {
				t.Fatal("picker shortcut returned a command")
			}
			if m.picker == nil {
				t.Fatal("picker shortcut did not open a picker")
			}
			if !strings.Contains(m.picker.heading, tt.wantHeading) {
				t.Errorf("picker heading = %q, want it to contain %q", m.picker.heading, tt.wantHeading)
			}
		})
	}
}

func TestHandleKey_EnterSubmitsWithoutOpeningModelPicker(t *testing.T) {
	m := newTestAppModel(t)
	oldSessionID := m.orch.CurrentSessionID()
	m.blocks = []string{"clear me"}
	m.lastKnownUsage.PromptTokens = 10
	m.queuedMessages = []string{"stale follow-up"}
	m.input.SetValue("/clear")

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd != nil {
		t.Fatal("/clear submission returned a command")
	}
	if m.picker != nil {
		t.Fatal("Enter opened a picker")
	}
	if len(m.blocks) != 0 {
		t.Errorf("Enter did not submit /clear; blocks = %v", m.blocks)
	}
	if got := m.orch.CurrentSessionID(); got == oldSessionID || got == "" {
		t.Errorf("/clear session ID = %q, want a new non-empty ID", got)
	}
	if m.lastKnownUsage.PromptTokens != 0 {
		t.Errorf("/clear retained last usage: %+v", m.lastKnownUsage)
	}
	if len(m.queuedMessages) != 0 {
		t.Errorf("/clear retained queued messages: %q", m.queuedMessages)
	}
}

func TestHandleModelCommand_InfersActiveProviderForUnknownLiveModel(t *testing.T) {
	m := newTestAppModel(t)

	m.handleModelCommand("new-live-model")

	provider, modelID := m.orch.ActiveModelInfo()
	if provider != "openai" || modelID != "new-live-model" {
		t.Errorf("active model = %s/%s, want openai/new-live-model", provider, modelID)
	}
}

func TestContextUsageSegmentUsesFinalModelCall(t *testing.T) {
	m := newTestAppModel(t)
	m.lastKnownUsage = model.Usage{
		PromptTokens:     315_000,
		CompletionTokens: 5_000,
		ContextTokens:    42_000,
	}

	got := m.contextUsageSegment()
	if got != "ctx 42.0k/128.0k (32%)" {
		t.Fatalf("contextUsageSegment() = %q, want final model call usage", got)
	}
}

func TestStreamingDoesNotForceViewportToBottomAfterPageUp(t *testing.T) {
	m := newTestAppModel(t)
	m.followOutput = true
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	m.blocks = []string{strings.Repeat("line\n", 40)}
	m.viewport.SetContent(m.renderTranscript())
	m.viewport.GotoBottom()

	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.followOutput || m.viewport.AtBottom() {
		t.Fatal("PageUp did not detach viewport from live output")
	}

	m.sending = true
	before := m.viewport.YOffset()
	_, _ = m.handleStreamDelta(streamDeltaMsg{
		resp: &model.ChatResponse{Content: "more output\n"},
		ch:   make(chan *model.ChatResponse),
	})
	if got := m.viewport.YOffset(); got != before {
		t.Errorf("stream moved viewport offset from %d to %d while detached", before, got)
	}
}

func TestStreamDeltaSchedulesBoundedRender(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	ch := make(chan *model.ChatResponse)

	_, _ = m.handleStreamDelta(streamDeltaMsg{resp: &model.ChatResponse{Content: "a"}, ch: ch})
	if !m.renderScheduled {
		t.Fatal("first delta did not schedule a render")
	}
	_, _ = m.handleStreamDelta(streamDeltaMsg{resp: &model.ChatResponse{Content: "b"}, ch: ch})
	if got := m.activeAgentText.String(); got != "ab" {
		t.Fatalf("streamed text = %q, want %q", got, "ab")
	}
}

func TestStreamDeltaPreservesConsecutiveIdenticalChunks(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	ch := make(chan *model.ChatResponse)

	_, _ = m.handleStreamDelta(streamDeltaMsg{resp: &model.ChatResponse{Content: "\n", Delta: true}, ch: ch})
	_, _ = m.handleStreamDelta(streamDeltaMsg{resp: &model.ChatResponse{Content: "\n", Delta: true}, ch: ch})
	if got := m.activeAgentText.String(); got != "\n\n" {
		t.Fatalf("streamed text = %q, want both identical deltas", got)
	}
}

func TestStreamDeltaDoesNotDuplicateCumulativeFrame(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	ch := make(chan *model.ChatResponse)

	_, _ = m.handleStreamDelta(streamDeltaMsg{resp: &model.ChatResponse{Content: "hello", Delta: true}, ch: ch})
	_, _ = m.handleStreamDelta(streamDeltaMsg{resp: &model.ChatResponse{Content: "hello world"}, ch: ch})
	if got := m.activeAgentText.String(); got != "hello world" {
		t.Fatalf("streamed text = %q, want cumulative frame merged once", got)
	}
}

func TestViewEnablesMouseWheelEvents(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Errorf("View().MouseMode = %v, want MouseModeCellMotion", got)
	}
}

func TestHelpDocumentsMouseAndNativeTerminalClipboard(t *testing.T) {
	for _, want := range []string{"mouse wheel", "pgup / pgdown", "shift+drag", "Native clipboard paste"} {
		if !strings.Contains(helpText, want) {
			t.Errorf("help text missing %q", want)
		}
	}
}

func TestPasteInsertsMultilineComposerWithoutSubmitting(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	_, cmd := m.Update(tea.PasteMsg{Content: "first line\nsecond line\nthird line"})

	if cmd != nil {
		t.Fatal("paste returned an unexpected command")
	}
	if got := m.input.Value(); got != "first line\nsecond line\nthird line" {
		t.Fatalf("pasted composer = %q", got)
	}
	if m.sending {
		t.Fatal("paste submitted the prompt")
	}
	if got := m.input.Height(); got != 3 {
		t.Fatalf("composer height = %d, want 3", got)
	}
}

func TestTranscriptNavigationResumesFollowAtBottom(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	m.blocks = []string{strings.Repeat("line\n", 40)}
	m.refreshViewport()

	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	if m.followOutput || !m.viewport.AtTop() {
		t.Fatal("Ctrl+Home did not detach at transcript top")
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	if !m.followOutput || !m.viewport.AtBottom() {
		t.Fatal("Ctrl+End did not resume live output")
	}
}

func TestCopyCommandRoundTrip(t *testing.T) {
	m := newTestAppModel(t)
	want := "first line\nUnicode: 世界\nlast line"
	var written string
	m.clipboardWrite = func(content string) error {
		written = content
		return nil
	}
	m.lastAssistantText = want
	m.input.SetValue("/copy")

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("/copy returned no clipboard command")
	}
	if m.statusMsg != "copying" {
		t.Errorf("statusMsg = %q, want copying", m.statusMsg)
	}
	_, _ = m.Update(cmd())
	if written != want {
		t.Fatalf("clipboard write = %q, want exact multiline Unicode", written)
	}
	if m.statusMsg != "copied response" {
		t.Errorf("statusMsg = %q, want copied response", m.statusMsg)
	}
}

func TestCopyShortcutFailureRoundTrip(t *testing.T) {
	m := newTestAppModel(t)
	m.lastAssistantText = "response to copy"
	m.clipboardWrite = func(string) error { return fmt.Errorf("host clipboard unavailable") }

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})

	if cmd == nil || m.statusMsg != "copying" {
		t.Fatalf("Ctrl+Y copy command = %v, status = %q", cmd != nil, m.statusMsg)
	}
	_, _ = m.Update(cmd())
	if m.statusMsg != "copy failed: host clipboard unavailable" {
		t.Fatalf("Ctrl+Y failure status = %q", m.statusMsg)
	}
}

func TestCopyCommandFailureRoundTrip(t *testing.T) {
	m := newTestAppModel(t)
	m.lastAssistantText = "response to copy"
	m.clipboardWrite = func(string) error { return fmt.Errorf("host clipboard unavailable") }
	m.input.SetValue("/copy")

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, _ = m.Update(cmd())
	if m.statusMsg != "copy failed: host clipboard unavailable" {
		t.Fatalf("/copy failure status = %q", m.statusMsg)
	}
}

func TestCopyShortcutSuccessRoundTrip(t *testing.T) {
	m := newTestAppModel(t)
	want := "response\n世界"
	var written string
	m.lastAssistantText = want
	m.clipboardWrite = func(content string) error { written = content; return nil }

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	_, _ = m.Update(cmd())
	if written != want || m.statusMsg != "copied response" {
		t.Fatalf("Ctrl+Y wrote %q with status %q", written, m.statusMsg)
	}
}

func TestPasteShortcutRoundTripPreservesExactMultilineUnicode(t *testing.T) {
	m := newTestAppModel(t)
	want := "first line\nUnicode: 世界\nlast line"
	m.input.SetValue("prefix: ")
	m.input.CursorEnd()
	m.clipboardRead = func() (string, error) { return want, nil }

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	if cmd == nil || m.statusMsg != "pasting" {
		t.Fatalf("Ctrl+V paste command = %v, status = %q", cmd != nil, m.statusMsg)
	}
	if got := m.input.Value(); got != "prefix: " {
		t.Fatalf("composer mutated before clipboard result: %q", got)
	}
	_, _ = m.Update(cmd())
	if got := m.input.Value(); got != "prefix: "+want {
		t.Fatalf("pasted composer = %q, want exact multiline Unicode", got)
	}
	if m.statusMsg != "pasted clipboard" {
		t.Fatalf("paste success status = %q", m.statusMsg)
	}
}

func TestPasteShortcutFailureDoesNotMutateComposer(t *testing.T) {
	m := newTestAppModel(t)
	m.input.SetValue("unchanged 世界")
	m.clipboardRead = func() (string, error) { return "ignored", fmt.Errorf("host clipboard unavailable") }

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	_, _ = m.Update(cmd())
	if got := m.input.Value(); got != "unchanged 世界" {
		t.Fatalf("failed paste mutated composer: %q", got)
	}
	if m.statusMsg != "paste failed: host clipboard unavailable" {
		t.Fatalf("paste failure status = %q", m.statusMsg)
	}
}

func TestClipboardShortcutsDoNotAccessClipboardWithOverlay(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*appModel)
	}{
		{name: "approval", setup: func(m *appModel) { m.approval = &pendingApproval{} }},
		{name: "wizard", setup: func(m *appModel) { m.wizard = &loginWizard{} }},
		{name: "picker", setup: func(m *appModel) { m.picker = &picker{} }},
		{name: "search", setup: func(m *appModel) { m.searching = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestAppModel(t)
			m.lastAssistantText = "response"
			tt.setup(m)
			accesses := 0
			m.clipboardRead = func() (string, error) { accesses++; return "clipboard", nil }
			m.clipboardWrite = func(string) error { accesses++; return nil }

			for _, msg := range []tea.KeyPressMsg{{Code: 'y', Mod: tea.ModCtrl}, {Code: 'v', Mod: tea.ModCtrl}} {
				_, cmd := m.Update(msg)
				if cmd != nil {
					t.Fatal("clipboard shortcut returned a command while overlay was active")
				}
			}
			if accesses != 0 || m.input.Value() != "" {
				t.Fatalf("overlay clipboard access = %d, composer = %q", accesses, m.input.Value())
			}
		})
	}
}

func TestCopyWithNoAssistantResponseDoesNotAccessClipboard(t *testing.T) {
	m := newTestAppModel(t)
	accesses := 0
	m.clipboardWrite = func(string) error { accesses++; return nil }

	for _, submit := range []func() tea.Cmd{
		func() tea.Cmd {
			m.input.SetValue("/copy")
			_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			return cmd
		},
		func() tea.Cmd {
			_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
			return cmd
		},
	} {
		if cmd := submit(); cmd != nil {
			t.Fatal("empty copy returned a clipboard command")
		}
	}
	if accesses != 0 || m.input.Value() != "" {
		t.Fatalf("empty copy access = %d, composer = %q", accesses, m.input.Value())
	}
	if m.statusMsg != "nothing to copy" {
		t.Fatalf("empty copy status = %q", m.statusMsg)
	}
}

func TestOrdinaryPromptKeepsPrimaryAgentActive(t *testing.T) {
	m := newTestAppModel(t)
	m.input.SetValue("please review and explain this code")

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("ordinary prompt returned no send command")
	}
	if got := m.orch.ActiveID(); got != m.orch.PrimaryID() {
		t.Errorf("ordinary prompt switched active agent to %q; primary is %q", got, m.orch.PrimaryID())
	}
}

func TestDefaultChromeDoesNotExposeInternalAgentOrModel(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})

	header := m.renderHeaderBar()
	status := m.renderStatusBar()
	if strings.Contains(header, "openai/") || strings.Contains(status, "coder") || strings.Contains(m.input.Prompt, "coder") {
		t.Errorf("default chrome exposes implementation details: header=%q status=%q prompt=%q", header, status, m.input.Prompt)
	}
}

func TestPrimaryAssistantTurnUsesProductName(t *testing.T) {
	m := newTestAppModel(t)
	m.appendTurnText("answer")
	m.followOutput = true
	m.finalizeTurn(nil)

	transcript := strings.Join(m.blocks, "\n")
	if strings.Contains(transcript, "coder") || !strings.Contains(transcript, "chronos-code") {
		t.Errorf("primary assistant transcript label = %q", transcript)
	}
}

func TestApprovalModalFitsSmallTerminalAndShowsAllChoices(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	responses := make(chan approvalDecision, 1)
	_, _ = m.Update(approvalRequestMsg{
		toolName: "spawn_subagent",
		args: map[string]any{
			"agent": "researcher",
			"task":  "analyze the project",
		},
		resp: responses,
	})

	view := m.View()
	for _, want := range []string{"Permission Required", "spawn_subagent", "once", "always tool", "all session"} {
		if !strings.Contains(view.Content, want) {
			t.Errorf("approval view missing %q: %q", want, view.Content)
		}
	}
	if got := lipgloss.Height(view.Content); got > 12 {
		t.Errorf("approval view height = %d, exceeds terminal height 12", got)
	}

	_, _ = m.Update(tea.KeyPressMsg{Code: 'A', Text: "A"})
	decision := <-responses
	if !decision.allow || !decision.all {
		t.Errorf("uppercase A decision = %+v, want allow all session", decision)
	}
	if got, want := m.viewport.Height(), 7; got != want {
		t.Errorf("viewport height after approval = %d, want %d", got, want)
	}
}

func TestStreamDeltaShowsSubagentToolCall(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.sending = true

	_, _ = m.handleStreamDelta(streamDeltaMsg{
		resp: &model.ChatResponse{ToolCalls: []model.ToolCall{{
			Name:      "spawn_subagent",
			Arguments: `{"agent":"researcher","task":"analyze"}`,
		}}},
		ch: make(chan *model.ChatResponse),
	})

	if got := m.renderTranscript(); !strings.Contains(got, "@researcher") || !strings.Contains(got, "working") {
		t.Errorf("stream transcript does not show subagent call: %q", got)
	}
	_, _ = m.handleStreamDelta(streamDeltaMsg{
		resp: &model.ChatResponse{Content: "synthesized result"},
		ch:   make(chan *model.ChatResponse),
	})
	if got := m.renderTranscript(); !strings.Contains(got, "1 subagent completed") || strings.Contains(got, "subagent running") {
		t.Errorf("stream transcript does not complete subagent progress: %q", got)
	}
}

func TestActivityShowsAgentToolLifecycle(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.sending = true
	ch := make(chan chronosstream.Event)

	_, _ = m.handleActivity(activityMsg{event: chronosstream.Event{
		Type: chronosstream.EventToolCall,
		Data: map[string]any{"agent": "researcher", "tool": "search", "args": map[string]any{"query": "tokens"}},
	}, ch: ch})
	_, _ = m.handleActivity(activityMsg{event: chronosstream.Event{
		Type: chronosstream.EventToolResult,
		Data: map[string]any{"agent": "researcher", "tool": "search"},
	}, ch: ch})

	got := m.renderTranscript()
	for _, want := range []string{"@researcher", "search", "done"} {
		if !strings.Contains(got, want) {
			t.Errorf("activity transcript missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "search · running") || strings.Count(got, " search ") != 1 {
		t.Errorf("activity lifecycle was not updated in place: %q", got)
	}
}

func TestActivityCollapsesRepeatedModelCalls(t *testing.T) {
	m := newTestAppModel(t)
	m.turnID = 1
	m.sending = true
	ch := make(chan chronosstream.Event)
	for i := 0; i < 3; i++ {
		_, _ = m.handleActivity(activityMsg{turnID: 1, event: chronosstream.Event{
			Type: chronosstream.EventModelCall,
			Data: map[string]any{"agent": "reviewer", "model": "anthropic"},
		}, ch: ch})
	}
	got := m.renderTranscript()
	if strings.Count(got, "model") != 1 || !strings.Contains(got, "3 calls") {
		t.Fatalf("model activity was not collapsed: %q", got)
	}
}

func TestSubagentStreamPreviewIsUpdatedByActivityWithoutDuplication(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.sending = true
	m.activityCh = make(chan chronosstream.Event)
	responses := make(chan *model.ChatResponse)

	_, _ = m.handleStreamDelta(streamDeltaMsg{resp: &model.ChatResponse{ToolCalls: []model.ToolCall{{
		ID: "call-1", Name: "spawn_subagent", Arguments: `{"agent":"researcher","task":"inspect routing"}`,
	}}}, ch: responses})
	_, _ = m.handleActivity(activityMsg{event: chronosstream.Event{
		Type: chronosstream.EventToolCall,
		Data: map[string]any{"agent": "coder", "id": "call-1", "tool": "spawn_subagent", "args": map[string]any{"agent": "researcher", "task": "inspect routing"}},
	}, ch: m.activityCh})
	_, _ = m.handleActivity(activityMsg{event: chronosstream.Event{
		Type: chronosstream.EventToolResult,
		Data: map[string]any{"agent": "coder", "id": "call-1", "tool": "spawn_subagent"},
	}, ch: m.activityCh})

	got := m.renderTranscript()
	if strings.Count(got, "@researcher") != 1 || !strings.Contains(got, "completed") || strings.Contains(got, "working") {
		t.Fatalf("subagent lifecycle was not updated in place: %q", got)
	}
}

func TestFinalizeTurnSettlesVisibleSubagentPreview(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.sending = true
	m.appendTurnActivity(RenderSubagentActivity("@coder ", map[string]any{"agent": "researcher", "task": "inspect routing"}, false, nil))
	m.appendTurnText("final answer")
	m.finalizeTurn(nil)

	got := m.renderTranscript()
	if !strings.Contains(got, "@researcher") || !strings.Contains(got, "completed") || strings.Contains(got, "working") {
		t.Fatalf("final transcript retained stale subagent state: %q", got)
	}
}

func TestLateActivityAfterFinalizationIsIgnored(t *testing.T) {
	m := newTestAppModel(t)
	m.turnID = 3
	m.sending = false
	_, cmd := m.handleActivity(activityMsg{turnID: 3, event: chronosstream.Event{
		Type: chronosstream.EventToolCall,
		Data: map[string]any{"agent": "researcher", "tool": "file_read"},
	}})
	if cmd != nil || len(m.activeTurnItems) != 0 {
		t.Fatalf("late activity mutated finalized turn: items=%#v", m.activeTurnItems)
	}
}

func TestActivityRefreshIsRateLimited(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.sending = true
	ch := make(chan chronosstream.Event)

	_, cmd := m.handleActivity(activityMsg{event: chronosstream.Event{
		Type: chronosstream.EventToolCall,
		Data: map[string]any{"agent": "coder", "id": "one", "tool": "file_read", "args": map[string]any{"path": "app.go"}},
	}, ch: ch})

	if cmd == nil || !m.renderScheduled {
		t.Fatal("activity did not schedule a bounded render tick")
	}
	if strings.Contains(m.viewport.View(), "file_read") {
		t.Fatal("activity refreshed the viewport before the scheduled render tick")
	}
}

func TestTurnTimelinePreservesNarrationAndToolOrder(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.sending = true
	m.appendTurnText("I will inspect the renderer.")
	m.appendTurnActivity(RenderToolActivity("@coder ", "file_read", map[string]any{"path": "app.go"}, true, nil))
	m.appendTurnText("The render loop rebuilds the transcript.")

	got := m.renderTranscript()
	first := strings.Index(got, "I will inspect")
	tool := strings.Index(got, "file_read")
	last := strings.Index(got, "The render loop")
	if first < 0 || tool <= first || last <= tool {
		t.Fatalf("turn chronology was not preserved: %q", got)
	}
	lines := strings.Split(got, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	compact := strings.Join(lines, "\n")
	if !strings.Contains(compact, "renderer.\n\n") || !strings.Contains(compact, "app.go\n\nThe render") {
		t.Fatalf("turn phases are not visually separated: %q", got)
	}
}

func TestActivityCorrelatesParallelToolsOutOfOrder(t *testing.T) {
	m := newTestAppModel(t)
	m.turnID = 1
	m.sending = true
	ch := make(chan chronosstream.Event)
	for _, event := range []chronosstream.Event{
		{Type: chronosstream.EventToolCall, Data: map[string]any{"agent": "coder", "id": "a", "tool": "read", "args": map[string]any{"path": "a.go"}}},
		{Type: chronosstream.EventToolCall, Data: map[string]any{"agent": "coder", "id": "b", "tool": "search", "args": map[string]any{"query": "token"}}},
		{Type: chronosstream.EventToolResult, Data: map[string]any{"agent": "coder", "id": "b", "tool": "search"}},
	} {
		_, _ = m.handleActivity(activityMsg{turnID: 1, ctx: context.Background(), event: event, ch: ch})
	}

	got := m.renderTranscript()
	if strings.Count(got, "read") != 1 || strings.Count(got, "search") != 1 || !strings.Contains(got, "read · running") || !strings.Contains(got, "search · done") {
		t.Fatalf("parallel activity state = %q", got)
	}
}

func TestHandleSlashSkillsListsDiscoveredCatalog(t *testing.T) {
	m := newTestAppModel(t)
	m.input.SetValue("/skills")

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd != nil {
		t.Fatal("/skills returned a command")
	}
	output := strings.Join(m.blocks, "\n")
	if !strings.Contains(output, "skills (") || !strings.Contains(output, "code-review") {
		t.Errorf("/skills output = %q, want discovered bundled skills", output)
	}
}

func TestSkillSlashInvocationStartsTurn(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})

	_, cmd := m.handleSubmit("/code-review review the current diff")

	if cmd == nil || !m.sending {
		t.Fatal("explicit skill invocation did not start a turn")
	}
	if got := strings.Join(m.blocks, "\n"); !strings.Contains(got, "/code-review review the current diff") {
		t.Fatalf("transcript did not retain explicit skill invocation: %q", got)
	}
	m.turnCancel()
}

func TestSubagentSlashInvocationStartsIsolatedTurn(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})

	_, cmd := m.handleSubmit("/subagent researcher inspect the renderer")

	if cmd == nil || !m.sending {
		t.Fatal("direct subagent invocation did not start a turn")
	}
	got := m.renderTranscript()
	if !strings.Contains(got, "subagent:researcher") || !strings.Contains(got, "· running") {
		t.Fatalf("direct subagent activity is not visible: %q", got)
	}
	m.turnCancel()
}

func TestDynamicSubagentSlashInvocationAcceptsToolSchemaJSON(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})

	_, cmd := m.handleSubmit(`/subagent {"task":"inspect","system_prompt":"Be precise","tools":["file_read"]}`)

	if cmd == nil || !m.sending {
		t.Fatal("dynamic subagent invocation did not start a turn")
	}
	args, ok := m.activityArgs["direct-subagent"].(map[string]any)
	if !ok || args["system_prompt"] != "Be precise" {
		t.Fatalf("dynamic subagent arguments = %#v", m.activityArgs["direct-subagent"])
	}
	m.turnCancel()
}

func TestHandleKey_TabCompletesSlashCommand(t *testing.T) {
	m := newTestAppModel(t)
	m.input.SetValue("/ag")

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})

	if cmd != nil {
		t.Fatal("Tab completion returned a command")
	}
	if got := m.input.Value(); got != "/agent" {
		t.Errorf("input after Tab = %q, want /agent", got)
	}
}

func TestHandleKey_TabCompletesSkillAndAgent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "skill", input: "/code-r", want: "/code-review"},
		{name: "agent mention", input: "@cod", want: "@coder "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestAppModel(t)
			m.input.SetValue(tt.input)
			_, cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
			if cmd != nil {
				t.Fatal("Tab completion returned a command")
			}
			if got := m.input.Value(); got != tt.want {
				t.Fatalf("completed input = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleKey_DownSelectsNextSlashCompletion(t *testing.T) {
	m := newTestAppModel(t)
	m.input.SetValue("/ag")

	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})

	if got := m.input.Value(); got != "/agents" {
		t.Errorf("input after Down+Tab = %q, want /agents", got)
	}
}

func TestView_ShowsSlashCommandCompletions(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.input.SetValue("/mod")
	m.resizeViewport()

	view := m.View()
	if !strings.Contains(view.Content, "tab complete") || !strings.Contains(view.Content, "/model") {
		t.Errorf("View() does not show slash completions: %q", view.Content)
	}
	if got, want := m.viewport.Height(), 24; got != want {
		t.Errorf("viewport height with completions = %d, want %d", got, want)
	}
}

func TestUpdate_KeyReleasesDoNotTriggerPressActions(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.KeyReleaseMsg
	}{
		{name: "Enter", msg: tea.KeyReleaseMsg{Code: tea.KeyEnter}},
		{name: "Alt+Enter", msg: tea.KeyReleaseMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}},
		{name: "Ctrl+A", msg: tea.KeyReleaseMsg{Code: 'a', Mod: tea.ModCtrl}},
		{name: "Ctrl+M", msg: tea.KeyReleaseMsg{Code: 'm', Mod: tea.ModCtrl}},
		{name: "Ctrl+/", msg: tea.KeyReleaseMsg{Code: '/', Mod: tea.ModCtrl}},
		{name: "Ctrl+C", msg: tea.KeyReleaseMsg{Code: 'c', Mod: tea.ModCtrl}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &appModel{cancel: func() {}, queuedMessages: []string{"existing"}}
			_, cmd := m.Update(tt.msg)
			if cmd != nil {
				t.Fatal("key release returned a command")
			}
			if m.quitting || m.picker != nil || len(m.queuedMessages) != 1 || m.queuedMessages[0] != "existing" {
				t.Fatalf("key release changed action state: quitting=%v picker=%v queuedMessages=%q", m.quitting, m.picker != nil, m.queuedMessages)
			}
		})
	}
}

func TestFinalizeTurn_DispatchesQueuedFollowUp(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	m.queuedMessages = []string{"follow-up message"}
	m.appendTurnText("first response")

	cmd := m.finalizeTurn(nil)

	if cmd == nil {
		t.Fatal("finalizeTurn() did not return the queued message command")
	}
	if len(m.queuedMessages) != 0 {
		t.Errorf("queuedMessages = %q after finalizeTurn, want empty", m.queuedMessages)
	}
	if !m.sending {
		t.Fatal("queued follow-up was not dispatched")
	}
	if got, ok := m.history.Prev(""); !ok || got != "follow-up message" {
		t.Errorf("history.Prev() = %q, %v; want queued follow-up", got, ok)
	}
}

func TestFinalizeTurn_BudgetErrorStopsQueuedRetries(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	m.budgetRetried = true
	m.queuedMessages = []string{"do not retry yet"}
	m.appendTurnActivity(RenderToolActivity("@coder ", "file_read", map[string]any{"path": "app.go"}, true, nil))

	cmd := m.finalizeTurn(fmt.Errorf("agent stream: token budget exceeded for session %q: used 10 of 10 tokens", "session"))

	if cmd != nil {
		t.Fatal("budget exhaustion dispatched a queued request")
	}
	transcript := strings.Join(m.blocks, "\n")
	if !strings.Contains(transcript, "Use /clear to start a fresh session") {
		t.Fatalf("budget error did not explain recovery: %q", m.blocks)
	}
	if !strings.Contains(transcript, "file_read") {
		t.Fatalf("failed turn discarded its activity timeline: %q", transcript)
	}
	if m.statusMsg != "budget exhausted │ /clear to continue" {
		t.Fatalf("statusMsg = %q", m.statusMsg)
	}
}

func TestFinalizeTurn_BudgetErrorCompactsAndRetriesInSameSession(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	m.activeRequest = "continue the task"
	m.lastKnownUsage.PromptTokens = 10
	oldSession := m.orch.CurrentSessionID()

	cmd := m.finalizeTurn(fmt.Errorf("token budget exceeded for session %q: used 10 of 10 tokens", oldSession))

	if cmd == nil || !m.sending || !m.budgetRetried {
		t.Fatal("budget exhaustion did not schedule one retry")
	}
	// Compaction (not a hard reset) is the default recovery path: the
	// session's conversation history is what's worth keeping, since a
	// budget cap is a cumulative-cost concern, not a context-window one.
	// With nothing yet persisted to this synthetic session, CompactSession
	// is a trivial no-op success, so the same session ID carries forward.
	if got := m.orch.CurrentSessionID(); got != oldSession {
		t.Fatalf("expected compaction to keep the same session: old=%q new=%q", oldSession, got)
	}
	if m.lastKnownUsage.PromptTokens != 0 {
		t.Fatalf("session recovery retained usage: %+v", m.lastKnownUsage)
	}
	if got := m.renderTranscript(); !strings.Contains(got, "compacting history and resuming") {
		t.Fatalf("automatic compaction is not visible in transcript: %q", got)
	}
	m.turnCancel()
}

func TestEnterWhileSendingInterruptsBeforeReplacement(t *testing.T) {
	m := newTestAppModel(t)
	canceled := false
	m.sending = true
	m.turnCancel = func() { canceled = true }
	m.input.SetValue("replacement prompt")

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd != nil {
		t.Fatal("replacement started before interrupted turn settled")
	}
	if !canceled || !m.turnInterrupted {
		t.Fatal("active turn was not interrupted")
	}
	if got := m.queuedMessages; len(got) != 1 || got[0] != "replacement prompt" {
		t.Fatalf("queued replacement = %q", got)
	}
}

func TestQueuedFollowUpsPreserveFIFOOrder(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	for _, prompt := range []string{"first", "second", "third"} {
		m.input.SetValue(prompt)
		_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})
	}

	if got, want := strings.Join(m.queuedMessages, "|"), "first|second|third"; got != want {
		t.Fatalf("queued order = %q, want %q", got, want)
	}
}

func TestCtrlCInterruptsActiveTurnBeforeQuitting(t *testing.T) {
	m := newTestAppModel(t)
	canceled := false
	m.sending = true
	m.turnCancel = func() { canceled = true }

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	if cmd != nil || m.quitting {
		t.Fatal("Ctrl+C quit while a turn was active")
	}
	if !canceled || m.statusMsg != "interrupting..." {
		t.Fatalf("Ctrl+C cancellation = %v, status = %q", canceled, m.statusMsg)
	}
}

func TestStaleStreamDeltaDoesNotMutateActiveTurn(t *testing.T) {
	m := newTestAppModel(t)
	m.turnID = 2
	m.activeAgentText.WriteString("current")

	_, cmd := m.handleStreamDelta(streamDeltaMsg{
		turnID: 1,
		resp:   &model.ChatResponse{Content: "stale"},
	})

	if cmd != nil || m.activeAgentText.String() != "current" {
		t.Fatalf("stale stream mutated active text: %q", m.activeAgentText.String())
	}
}

func TestResponsiveChromeFitsTerminalWidth(t *testing.T) {
	for _, width := range []int{120, 80, 40, 20} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			m := newTestAppModel(t)
			m.workDir = "/a/very/long/workspace/path/that/must/not/wrap/the/header"
			m.statusMsg = "a long status message that must degrade before wrapping"
			m.sending = true
			m.queuedMessages = []string{"one", "two"}
			_, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 24})

			for name, line := range map[string]string{
				"header": m.renderHeaderBar(),
				"status": m.renderStatusBar(),
			} {
				if got := lipgloss.Width(line); got > width {
					t.Fatalf("%s width = %d, terminal width = %d: %q", name, got, width, line)
				}
			}
		})
	}
}

func TestNarrowViewStaysWithinTerminalBounds(t *testing.T) {
	for _, size := range []struct{ width, height int }{{40, 10}, {20, 6}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			m := newTestAppModel(t)
			_, _ = m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			m.sending = true
			m.appendTurnActivity(RenderToolCall("very_long_tool_name", strings.Repeat("x", 100)))
			m.appendTurnText("A response that must wrap within a narrow terminal pane.")
			m.refreshViewport()

			view := m.View().Content
			for i, line := range strings.Split(view, "\n") {
				if got := lipgloss.Width(line); got > size.width {
					t.Fatalf("line %d width = %d, terminal width = %d: %q", i, got, size.width, line)
				}
			}
			if got := lipgloss.Height(view); got > size.height {
				t.Fatalf("view height = %d, terminal height = %d", got, size.height)
			}
		})
	}
}

func TestUnicodeBackspaceRemovesWholeRune(t *testing.T) {
	if got := removeLastRune("model界"); got != "model" {
		t.Fatalf("removeLastRune() = %q, want model", got)
	}
}

func TestUnsupportedKeyboardEnhancements_HasSlashModelFallback(t *testing.T) {
	m := newTestAppModel(t)
	m.input.SetValue("/model definitely-not-a-model")

	_, cmd := m.Update(tea.KeyboardEnhancementsMsg{})
	if cmd != nil {
		t.Fatal("unsupported keyboard enhancement state returned a command")
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("slash fallback returned a command")
	}
	if m.picker != nil {
		t.Fatal("slash fallback unexpectedly opened a picker")
	}
	if got := strings.Join(m.blocks, "\n"); !strings.Contains(got, "definitely-not-a-model") {
		t.Errorf("slash fallback was not handled; transcript = %q", got)
	}
	if !strings.Contains(helpText, "use /model or ctrl+/ if terminal key enhancements are unavailable") {
		t.Fatal("/help does not document the unsupported-terminal fallback")
	}
}

func TestRenderTranscript_EmptyBlocks(t *testing.T) {
	m := &appModel{}
	got := m.renderTranscript()
	if got != "" {
		t.Errorf("renderTranscript() with no blocks = %q, want empty", got)
	}
}

func TestInstallApprovalHandlersDelegatesToCompositionOwner(t *testing.T) {
	stub := &approvalInstallerStub{}
	wantCalled := false
	handler := func(context.Context, string, map[string]any) (bool, error) {
		wantCalled = true
		return true, nil
	}
	installApprovalHandlers(stub, handler)
	if stub.calls != 1 || stub.handler == nil {
		t.Fatalf("SetApprovalHandler calls = %d, handler nil = %v; want 1, false", stub.calls, stub.handler == nil)
	}
	if _, err := stub.handler(context.Background(), "shell", nil); err != nil {
		t.Fatalf("installed handler error = %v", err)
	}
	if !wantCalled {
		t.Fatal("installApprovalHandlers did not pass through the TUI handler")
	}
}
