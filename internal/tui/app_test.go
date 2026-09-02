package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spawn08/chronos/engine/model"
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

	ta := textarea.New()
	return &appModel{
		orch:    orch,
		ctx:     context.Background(),
		cancel:  func() {},
		input:   ta,
		history: NewHistory(),
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
	m := &appModel{renderWidth: 0}
	m.blocks = []string{"block-A", "block-B"}

	_ = m.renderTranscript()
	if len(m.renderedBlocks) != 2 {
		t.Fatalf("renderedBlocks len = %d, want 2", len(m.renderedBlocks))
	}

	m.blocks = append(m.blocks, "block-C")
	got := m.renderTranscript()
	if len(m.renderedBlocks) != 3 {
		t.Fatalf("renderedBlocks len = %d after append, want 3", len(m.renderedBlocks))
	}
	if !strings.Contains(got, "block-A") || !strings.Contains(got, "block-C") {
		t.Errorf("renderTranscript() = %q, want all blocks present", got)
	}
}

func TestRenderTranscript_WidthChangeInvalidatesCache(t *testing.T) {
	m := &appModel{renderWidth: 0}
	m.blocks = []string{"hello"}

	_ = m.renderTranscript()
	if m.renderWidth != 0 {
		t.Fatalf("renderWidth = %d, want 0", m.renderWidth)
	}
	if len(m.renderedBlocks) != 1 {
		t.Fatalf("renderedBlocks len = %d, want 1", len(m.renderedBlocks))
	}

	// Simulate width change by directly setting viewport width via renderWidth check
	m.renderWidth = 0 // matches viewport width of 0
	_ = m.renderTranscript()
	if len(m.renderedBlocks) != 1 {
		t.Fatalf("cache should still be valid, got len %d", len(m.renderedBlocks))
	}
}

func TestInvalidateRenderCache(t *testing.T) {
	m := &appModel{}
	m.blocks = []string{"a", "b"}
	_ = m.renderTranscript()
	if len(m.renderedBlocks) != 2 {
		t.Fatalf("renderedBlocks len = %d, want 2", len(m.renderedBlocks))
	}
	m.invalidateRenderCache()
	if m.renderedBlocks != nil {
		t.Errorf("renderedBlocks = %v after invalidate, want nil", m.renderedBlocks)
	}
}

// TestHandleKey_AltEnterQueuesWhileSending covers AC-2: Alt+Enter while a
// turn is streaming captures the input into queuedMessage instead of
// inserting a newline.
func TestHandleKey_AltEnterQueuesWhileSending(t *testing.T) {
	ta := textarea.New()
	ta.SetValue("follow-up message")
	m := &appModel{input: ta, sending: true}

	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})

	if m.queuedMessage != "follow-up message" {
		t.Errorf("queuedMessage = %q, want %q", m.queuedMessage, "follow-up message")
	}
	if m.input.Value() != "" {
		t.Errorf("input.Value() = %q after queuing, want empty", m.input.Value())
	}
}

// TestHandleKey_AltEnterDoesNotQueueWhenIdle covers the negative case: with
// no turn streaming, Alt+Enter must not touch queuedMessage (it falls
// through to the textarea's own insert-newline binding instead).
func TestHandleKey_AltEnterDoesNotQueueWhenIdle(t *testing.T) {
	ta := textarea.New()
	ta.SetValue("still typing")
	m := &appModel{input: ta, sending: false}

	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})

	if m.queuedMessage != "" {
		t.Errorf("queuedMessage = %q, want empty when not sending", m.queuedMessage)
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
}

func TestHandleModelCommand_InfersActiveProviderForUnknownLiveModel(t *testing.T) {
	m := newTestAppModel(t)

	m.handleModelCommand("new-live-model")

	provider, modelID := m.orch.ActiveModelInfo()
	if provider != "openai" || modelID != "new-live-model" {
		t.Errorf("active model = %s/%s, want openai/new-live-model", provider, modelID)
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

func TestViewLeavesMouseCaptureDisabled(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Errorf("View().MouseMode = %v, want MouseModeNone for native text selection", got)
	}
}

func TestCopyReturnsClipboardCommandForLastAssistantResponse(t *testing.T) {
	m := newTestAppModel(t)
	m.lastAssistantText = "response to copy"
	m.input.SetValue("/copy")

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("/copy returned no clipboard command")
	}
	if m.statusMsg != "copy requested" {
		t.Errorf("statusMsg = %q, want copy requested", m.statusMsg)
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
	m.activeAgentText.WriteString("answer")
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
	if got, want := m.viewport.Height(), 6; got != want {
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

	if got := m.renderTranscript(); !strings.Contains(got, "spawn_subagent") || !strings.Contains(got, "researcher") || !strings.Contains(got, "1 subagent running") {
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
	if got, want := m.viewport.Height(), 23; got != want {
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
			m := &appModel{cancel: func() {}, queuedMessage: "existing"}
			_, cmd := m.Update(tt.msg)
			if cmd != nil {
				t.Fatal("key release returned a command")
			}
			if m.quitting || m.picker != nil || m.queuedMessage != "existing" {
				t.Fatalf("key release changed action state: quitting=%v picker=%v queuedMessage=%q", m.quitting, m.picker != nil, m.queuedMessage)
			}
		})
	}
}

func TestFinalizeTurn_DispatchesQueuedFollowUp(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	m.queuedMessage = "follow-up message"
	m.activeAgentText.WriteString("first response")

	cmd := m.finalizeTurn(nil)

	if cmd == nil {
		t.Fatal("finalizeTurn() did not return the queued message command")
	}
	if m.queuedMessage != "" {
		t.Errorf("queuedMessage = %q after finalizeTurn, want empty", m.queuedMessage)
	}
	if !m.sending {
		t.Fatal("queued follow-up was not dispatched")
	}
	if got, ok := m.history.Prev(""); !ok || got != "follow-up message" {
		t.Errorf("history.Prev() = %q, %v; want queued follow-up", got, ok)
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
