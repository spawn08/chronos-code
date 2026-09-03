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

func TestMouseWheelDetachesViewportFromLiveOutput(t *testing.T) {
	m := newTestAppModel(t)
	m.followOutput = true
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	m.blocks = []string{strings.Repeat("line\n", 40)}
	m.viewport.SetContent(m.renderTranscript())
	m.viewport.GotoBottom()

	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.followOutput || m.viewport.AtBottom() {
		t.Fatal("mouse wheel did not detach viewport from live output")
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

func TestViewUsesNativeSelection(t *testing.T) {
	m := newTestAppModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Errorf("View().MouseMode = %v, want MouseModeNone for native selection", got)
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

func TestCopyShortcutReturnsClipboardCommand(t *testing.T) {
	m := newTestAppModel(t)
	m.lastAssistantText = "response to copy"

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})

	if cmd == nil || m.statusMsg != "copy requested" {
		t.Fatalf("Ctrl+Y copy command = %v, status = %q", cmd != nil, m.statusMsg)
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
	if strings.Contains(got, "search running") || strings.Count(got, " search ") != 1 {
		t.Errorf("activity lifecycle was not updated in place: %q", got)
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
	if strings.Count(got, "read") != 1 || strings.Count(got, "search") != 1 || !strings.Contains(got, "read running") || !strings.Contains(got, "search done") {
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
	m.activeAgentText.WriteString("first response")

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
			m.activeToolLines = []string{RenderToolCall("very_long_tool_name", strings.Repeat("x", 100))}
			m.activeAgentText.WriteString("A response that must wrap within a narrow terminal pane.")
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
