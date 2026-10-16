package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	chronosstream "github.com/spawn08/chronos/engine/stream"

	"github.com/spawn08/chronos-code/internal/orchestrator"
)

func TestBusyFooterPersistsAfterTranscriptItemsAndWhileScrolled(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.sending = true
	m.workLabel = "file_read"
	m.busyStarted = time.Now().Add(-3 * time.Second)
	m.appendTurnText(strings.Repeat("existing transcript\n", 60))
	m.refreshViewportNow()
	m.followOutput = false
	m.viewportFrozen = true
	m.viewport.GotoTop()
	m.viewportViewValid = false
	before := m.transcriptView()
	tick := m.startSpinner()
	if tick == nil {
		t.Fatal("spinner did not start")
	}
	beforeStatus := ansi.Strip(m.renderStatusBar())
	_, next := m.Update(tick())
	afterStatus := ansi.Strip(m.renderStatusBar())
	if next == nil || beforeStatus == afterStatus {
		t.Fatal("spinner stopped after transcript content or scrolling")
	}
	if !strings.Contains(afterStatus, "file_read") || !strings.Contains(afterStatus, "3s") {
		t.Fatalf("footer lacks activity/elapsed time: %s", afterStatus)
	}
	if m.transcriptView() != before || !m.viewportViewValid || !m.viewportFrozen {
		t.Fatal("spinner reflowed the frozen transcript")
	}
	if m.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("spinner changed mouse capture")
	}
	m.sending = false
	_, next = m.Update(spinner.TickMsg{})
	if next != nil || m.spinnerRunning {
		t.Fatal("idle spinner kept running")
	}
}

func TestBusyFooterSurvivesLongStatusAtSmallWidths(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	m.workLabel = "file_read"
	m.busyStarted = time.Now().Add(-4 * time.Second)
	m.statusMsg = strings.Repeat("long provider status\n", 30)
	for _, width := range []int{24, 40, 80, 160} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		bar := ansi.Strip(m.renderStatusBar())
		if !strings.Contains(bar, "file_read") || !strings.Contains(bar, "4s") {
			t.Fatalf("width %d hid activity: %s", width, bar)
		}
		if lipgloss.Width(bar) > width || strings.Contains(bar, "\n") {
			t.Fatalf("width %d overflowed: %s", width, bar)
		}
	}
}

func TestQuestionWaitingStatusAndSpinnerResume(t *testing.T) {
	m, _, _ := showTestQuestion(t, []orchestrator.UserQuestion{{Question: "Choose?", Options: []string{"Go", "Rust"}}})
	m.sending = true
	m.workLabel = "ask_user_question"
	m.startSpinner()
	_, cmd := m.Update(spinner.TickMsg{})
	if cmd != nil || m.spinnerRunning {
		t.Fatal("spinner kept animating while waiting for an answer")
	}
	if bar := ansi.Strip(m.renderStatusBar()); !strings.Contains(bar, "awaiting answer") {
		t.Fatalf("waiting state missing: %s", bar)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, cmd = m.Update(operationalSnapshotMsg{})
	if cmd == nil || !m.spinnerRunning {
		t.Fatal("spinner did not restart after answer")
	}
}

func TestMaintenanceAndShellBusyStatus(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.maintenanceCmd("compacting session", nil)
	if bar := ansi.Strip(m.renderStatusBar()); !strings.Contains(bar, "compacting session") || !strings.Contains(bar, "0s") {
		t.Fatalf("maintenance state missing: %s", bar)
	}
	m.sending = false
	m.shellPending = 1
	m.workLabel = ""
	if bar := ansi.Strip(m.renderStatusBar()); !strings.Contains(bar, "shell") || strings.Contains(bar, "idle") {
		t.Fatalf("shell state missing: %s", bar)
	}
	_, cmd := m.Update(operationalSnapshotMsg{})
	if cmd == nil || !m.spinnerRunning {
		t.Fatal("shell did not animate")
	}
}

func TestToolActivityUpdatesBusyFooterAndInstructionDeferrals(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.sending = true
	data := map[string]any{"agent": "coder", "id": "read-1", "tool": "file_read", "args": map[string]any{"path": "src/CLAUDE.md"}}
	m.handleActivity(activityMsg{event: chronosstream.Event{Type: chronosstream.EventToolCall, Data: data}})
	if m.workLabel != "file_read" {
		t.Fatalf("activity = %s", m.workLabel)
	}
	data["error"] = "hook before tool file_read: scoped_instructions_needed: file access is deferred."
	m.handleActivity(activityMsg{event: chronosstream.Event{Type: chronosstream.EventToolResult, Data: data}})
	view := ansi.Strip(m.renderTranscript())
	if strings.Contains(view, "✗") || strings.Contains(view, "· failed") || !strings.Contains(view, "deferred · loading project instructions") {
		t.Fatalf("deferral shown as failure: %s", view)
	}
	if m.workLabel != "loading project instructions" {
		t.Fatalf("activity = %s", m.workLabel)
	}
	if !strings.Contains(m.activeTurnItems[0].failure, "scoped_instructions_needed") {
		t.Fatal("deferral detail was lost")
	}
	m.handleActivity(activityMsg{event: chronosstream.Event{Type: chronosstream.EventModelCall, Data: map[string]any{"agent": "coder"}}})
	if m.workLabel != "thinking" {
		t.Fatalf("model state = %s", m.workLabel)
	}
}
