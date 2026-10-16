package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/storage"
)

func seedVisibleHistory(t *testing.T, m *appModel, id string) {
	t.Helper()
	if err := m.orch.SessionManager().Ensure(m.ctx, id, m.orch.ActiveID()); err != nil {
		t.Fatal(err)
	}
	for i, event := range []*storage.Event{
		{Type: "chat_message", Payload: map[string]any{"role": "user", "content": "earlier question"}},
		{Type: "chat_message", Payload: map[string]any{"role": "assistant", "content": "earlier answer"}},
		{Type: "chat_message", Payload: map[string]any{"role": "assistant", "content": "", "tool_calls": []any{}}},
		{Type: "chat_message", Payload: map[string]any{"role": "tool", "content": "private tool result"}},
		{Type: "chat_summary", Payload: map[string]any{"version": 1, "covered_seq": 4, "summary": "internal summary", "preserved_messages": []any{}}},
		{Type: "chat_message", Payload: map[string]any{"role": "user", "content": "latest question"}},
		{Type: "chat_message", Payload: map[string]any{"role": "assistant", "content": "latest answer"}},
	} {
		event.ID = fmt.Sprintf("%s-event-%d", id, i)
		event.SessionID = id
		event.SeqNum = int64(i + 1)
		if err := m.orch.Store().AppendEvent(m.ctx, event); err != nil {
			t.Fatal(err)
		}
	}
}

func assertVisibleHistory(t *testing.T, m *appModel) {
	t.Helper()
	text := ansi.Strip(m.renderTranscript())
	previous := -1
	for _, message := range []string{"earlier question", "earlier answer", "latest question", "latest answer"} {
		at := strings.Index(text, message)
		if at <= previous || strings.Count(text, message) != 1 {
			t.Fatalf("history missing, duplicated, or out of order for %q: %s", message, text)
		}
		previous = at
	}
	for _, hidden := range []string{"private tool result", "internal summary", "unrelated conversation"} {
		if strings.Contains(text, hidden) {
			t.Fatalf("unexpected history %q: %s", hidden, text)
		}
	}
	if m.lastAssistantText != "latest answer" {
		t.Fatalf("copy response = %q", m.lastAssistantText)
	}
	last, _, err := m.copyText("last")
	if err != nil || last != "latest answer" {
		t.Fatalf("copy last = %q, %v", last, err)
	}
	all, _, err := m.copyText("all")
	if err != nil || !strings.Contains(all, "earlier question") || !strings.Contains(all, "latest answer") {
		t.Fatalf("copy transcript = %q, %v", all, err)
	}
}

func TestStartupResumeRestoresHistoryOnce(t *testing.T) {
	m := newTestAppModel(t)
	id := m.orch.CurrentSessionID()
	seedVisibleHistory(t, m, id)
	m.announcedSession = ""
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	assertVisibleHistory(t, m)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 14})
	m.Update(operationalSnapshotMsg{})
	assertVisibleHistory(t, m)
	if !m.viewport.AtBottom() {
		t.Fatal("restored transcript did not follow the latest message")
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	if !m.viewport.AtTop() {
		t.Fatal("restored history could not scroll to the first message")
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	if !m.viewport.AtBottom() {
		t.Fatal("restored history could not return to the latest message")
	}
	events, err := m.orch.Store().ListEvents(m.ctx, id, 0)
	if err != nil || len(events) != 7 {
		t.Fatalf("display changed saved events: %d, %v", len(events), err)
	}
}

func TestSlashResumeReplacesHistoryAndClearsEmptySession(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	const id = "history-target"
	seedVisibleHistory(t, m, id)
	m.appendUserTurn("unrelated conversation")
	m.hasLastTurn = true
	m.lastTurnBlockIdx = len(m.blocks) - 1
	m.lastTurnItems = []turnItem{{kind: turnItemText, content: "unrelated conversation"}}
	m.handleSubmit("/resume " + id)
	assertVisibleHistory(t, m)
	m.handleSubmit("/resume " + id)
	assertVisibleHistory(t, m)
	const empty = "empty-history-target"
	if err := m.orch.SessionManager().Ensure(m.ctx, empty, m.orch.ActiveID()); err != nil {
		t.Fatal(err)
	}
	m.handleSubmit("/resume " + empty)
	if strings.Contains(ansi.Strip(m.renderTranscript()), "latest answer") || m.lastAssistantText != "" || m.hasLastTurn {
		t.Fatal("empty session retained the previous conversation")
	}
}

func TestSessionPickerResumeRestoresHistory(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	const id = "picker-history-target"
	seedVisibleHistory(t, m, id)
	m.appendUserTurn("unrelated conversation")
	cmd := m.openSessionPicker()
	m.picker.filter = id
	m.Update(cmd())
	_, cmd = m.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("picker did not return a resume command")
	}
	m.Update(cmd())
	assertVisibleHistory(t, m)
	if m.sending || m.announcedSession != id {
		t.Fatal("picker did not finish restoring the session")
	}
}

func TestBareResumeRestoresPriorHistory(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	seedVisibleHistory(t, m, "prior-history-target")
	m.handleSubmit("/resume")
	assertVisibleHistory(t, m)
}

func TestRestoredHistorySupportsNextTurn(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	seedVisibleHistory(t, m, "continue-history-target")
	m.handleSubmit("/resume continue-history-target")
	m.appendUserTurn("follow-up question")
	m.activeTurnItems = []turnItem{{kind: turnItemText, content: "follow-up answer"}}
	m.activeAgentText.WriteString("follow-up answer")
	m.finalizeTurn(nil)
	text := ansi.Strip(m.renderTranscript())
	for _, message := range []string{"earlier question", "latest answer", "follow-up question", "follow-up answer"} {
		if !strings.Contains(text, message) {
			t.Fatalf("continued turn lost %q: %s", message, text)
		}
	}
	if m.lastAssistantText != "follow-up answer" || m.lastKnownUsage != (model.Usage{}) {
		t.Fatal("continued turn retained stale response state")
	}
}
