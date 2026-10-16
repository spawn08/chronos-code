package tui

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/spawn08/chronos-code/internal/modelinfo"
)

func TestTUIProgramLocalControlsDuringStream(t *testing.T) {
	m := newTestAppModel(t)
	m.followOutput = true
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.sending = true
	m.lastAssistantText = "previous response"
	m.appendTurnText("live response 世界")
	m.refreshViewport()
	copied := make(chan string, 4)
	m.clipboardWrite = func(text string) error { copied <- text; return nil }
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	program := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(io.Discard), tea.WithEnvironment([]string{"TERM=xterm-256color"}), tea.WithoutSignalHandler())
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	defer program.Kill()
	write := func(text string) {
		t.Helper()
		written := make(chan error, 1)
		go func() { _, err := io.WriteString(writer, text); written <- err }()
		select {
		case err := <-written:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("TUI input stopped responding")
		}
	}
	awaitCopy := func() {
		t.Helper()
		select {
		case text := <-copied:
			if text != "live response 世界" {
				t.Fatalf("copied %q", text)
			}
		case <-ctx.Done():
			t.Fatal("TUI copy stopped responding")
		}
	}
	write("/copy\r")
	awaitCopy()
	write("/mouse\r\x19")
	awaitCopy()
	write("/quit\r")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("TUI quit stopped responding")
	}
	if m.mouseCapture || len(m.queuedMessages) > 0 || !m.quitting {
		t.Fatal("local controls did not execute through the terminal event loop")
	}
}

func TestLocalUICommandsRunDuringTurn(t *testing.T) {
	for _, command := range []string{"/copy", "/copy visible", "/mouse", "/perf", "/help", "/quit"} {
		t.Run(command, func(t *testing.T) {
			m := newTestAppModel(t)
			m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
			m.appendSystem("visible transcript")
			m.refreshViewport()
			m.sending = true
			m.lastAssistantText = "answer"
			m.appendTurnText("current answer")
			m.input.SetValue(command)
			_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if len(m.queuedMessages) != 0 {
				t.Fatalf("local command was queued: %v", m.queuedMessages)
			}
			if strings.HasPrefix(command, "/copy") && cmd == nil {
				t.Fatal("copy did not dispatch")
			}
			if command == "/mouse" && m.mouseCapture {
				t.Fatal("mouse command did not take effect")
			}
			if command == "/quit" && !m.quitting {
				t.Fatal("quit did not take effect")
			}
		})
	}
}

func TestLiveModelPickerResultIsRenderedImmediately(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.picker = newModelPicker(m)
	m.picker.filter = "fresh-deployment"
	m.picker.applyFilter()
	m.resizeViewport()
	m.Update(modelPickerLiveMsg{results: []providerModelsResult{{provider: "azure", ok: true, models: []modelinfo.Info{{Provider: "azure", Model: "fresh-deployment"}}}}})
	if !strings.Contains(m.View().Content, "fresh-deployment") {
		t.Fatal("live picker result was absent from the rendered frame")
	}
}

func TestClipboardFeedbackVisibleAtNarrowWidths(t *testing.T) {
	for _, width := range []int{30, 40, 60, 80, 120} {
		m := newTestAppModel(t)
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		m.statusMsg = "copy failed: clipboard unavailable"
		if got := ansi.Strip(m.renderStatusBar()); !strings.Contains(got, "copy failed") {
			t.Fatalf("feedback hidden at width %d: %q", width, got)
		}
	}
}

func TestLiveTextCacheIntegrationInvalidatesAndSettles(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.sending = true
	m.appendTurnText("first line\nsecond")
	m.renderTurnItems()
	m.appendTurnText(" line\nthird")
	if got := m.renderTurnItems(); got != wrapText(m.activeTurnItems[0].content, 80) {
		t.Fatal("integrated live renderer differs from wrapping")
	}
	if m.activeTurnItems[0].liveText.completed == "" {
		t.Fatal("completed lines were not cached")
	}
	if cloneTurnItems(m.activeTurnItems)[0].liveText.valid {
		t.Fatal("snapshot retained live-only cache")
	}
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 24})
	if got := m.renderTurnItems(); got != wrapText(m.activeTurnItems[0].content, 20) {
		t.Fatal("resize retained old wrapping")
	}
	m.finalizeTurn(nil)
	if m.lastTurnItems[0].liveText.valid {
		t.Fatal("settled transcript retained live-only cache")
	}
}

func TestCopyDoesNotReturnOldAnswerBeforeFirstDelta(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	m.lastAssistantText = "previous answer\n```go\nold code\n```"
	for _, arg := range []string{"", "current"} {
		if copied, _, err := m.copyText(arg); err == nil || copied != "" {
			t.Fatalf("copy %q returned stale text %q", arg, copied)
		}
	}
	if copied, _, err := m.copyText("last"); err != nil || copied != m.lastAssistantText {
		t.Fatal("explicit last response became unavailable")
	}
	if copied, _, err := m.copyText("code"); err != nil || copied != "old code" {
		t.Fatal("latest available code block became unavailable")
	}
}

func TestLiveTextFallbackKeepsPlainWrapping(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 24})
	m.sending = true
	for _, text := range []string{"single line words with no newline", "\x1b[31mred words\nnext line\x1b[0m"} {
		item := turnItem{kind: turnItemText, content: text}
		if got := m.renderOneItem(&item); got != wrapText(text, 20) || item.liveText.valid {
			t.Fatal("fallback changed wrapping or retained an incremental cache")
		}
	}
}

func TestCopyTargetsCurrentStreamAndExplicitLast(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	m.lastAssistantText = "old answer\n```go\nold code\n```"
	m.appendTurnText("current answer\n```go\nnew code\n```\n" + strings.Repeat("世界", 40000))
	for _, arg := range []string{"", "current", "response"} {
		got, status, err := m.copyText(arg)
		if err != nil || got != m.activeAgentText.String() || status != "copied current response" {
			t.Fatalf("copy %q: %q, %v", arg, status, err)
		}
	}
	got, _, err := m.copyText("last")
	if err != nil || got != m.lastAssistantText {
		t.Fatal("explicit last did not retain completed response")
	}
	got, _, err = m.copyText("code")
	if err != nil || got != "new code" {
		t.Fatalf("current code = %q, %v", got, err)
	}
}

func TestCopyShortcutTerminalEncodingsAndInspectionAlias(t *testing.T) {
	for _, raw := range []string{"\x19", "\x1b[99;6u", "\x1b[27;6;67~"} {
		for _, inspection := range []bool{false, true} {
			m := newTestAppModel(t)
			m.lastAssistantText = "answer 世界"
			want := m.lastAssistantText
			if inspection {
				m.openInspection("details", "inspection 世界")
				want = m.inspection.content
			}
			var copied string
			m.clipboardWrite = func(text string) error { copied = text; return nil }
			var decoder uv.EventDecoder
			_, event := decoder.Decode([]byte(raw))
			decoded, ok := event.(uv.KeyPressEvent)
			if !ok {
				t.Fatalf("decoded %T", event)
			}
			_, cmd := m.Update(tea.KeyPressMsg(decoded))
			if cmd == nil {
				t.Fatalf("no copy command for %q inspection=%t", raw, inspection)
			}
			m.Update(cmd())
			if copied != want {
				t.Fatalf("copied %q, want %q", copied, want)
			}
		}
	}
}

func TestCopyVisiblePreservesScrolledViewport(t *testing.T) {
	m := newTestAppModel(t)
	m.followOutput = true
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	m.appendSystem(strings.Repeat("history line\n", 80))
	m.refreshViewport()
	m.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	before, offset := m.viewport.View(), m.viewport.YOffset()
	m.input.SetValue("/copy visible")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || m.viewport.View() != before || m.viewport.YOffset() != offset || m.followOutput {
		t.Fatal("copy changed the reading position")
	}
}

func TestFrozenTranscriptSurvivesCompletionAndRefresh(t *testing.T) {
	m := newTestAppModel(t)
	m.followOutput = true
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	m.sending = true
	m.appendTurnText(strings.Repeat("stream line\n", 80))
	m.refreshViewport()
	m.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	before := m.viewport.View()
	m.appendTurnText("LATEST_MARKER")
	m.refreshViewport()
	m.finalizeTurn(nil)
	if m.viewport.View() != before || !m.viewportDirty || m.followOutput {
		t.Fatal("completion repainted the frozen transcript")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	if !m.followOutput || m.viewportDirty || !strings.Contains(m.viewport.View(), "LATEST_MARKER") {
		t.Fatal("resume did not display buffered output")
	}
}

func TestClipboardPasteRejectsChangedFocusAndStaleTurn(t *testing.T) {
	for _, state := range []string{"inspection", "picker", "approval", "search", "turn", "new request"} {
		t.Run(state, func(t *testing.T) {
			m := newTestAppModel(t)
			m.input.SetValue("keep")
			m.clipboardRead = func() (string, error) { return "late paste", nil }
			_, cmd := m.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
			result := cmd()
			switch state {
			case "inspection":
				m.openInspection("details", "inspection")
			case "picker":
				m.picker = &picker{}
			case "approval":
				m.approval = &pendingApproval{}
			case "search":
				m.searching = true
			case "turn":
				m.turnID++
			case "new request":
				m.clipboardRequestID++
			}
			m.Update(result)
			if m.input.Value() != "keep" {
				t.Fatal("late paste changed the composer")
			}
		})
	}
}
