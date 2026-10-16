package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestSessionAnnouncementOnStartupAndResize(t *testing.T) {
	m := newTestAppModel(t)
	m.announcedSession = ""
	id := m.orch.CurrentSessionID()
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	text := ansi.Strip(m.renderTranscript())
	if !strings.Contains(text, "Session: "+id) || !strings.Contains(text, "chronos-code --resume "+id) {
		t.Fatalf("startup hint missing: %s", text)
	}
	count := len(m.blocks)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 14})
	m.Update(operationalSnapshotMsg{})
	if len(m.blocks) != count {
		t.Fatal("resize or heartbeat repeated the session announcement")
	}
	if !strings.Contains(ansi.Strip(m.renderTranscript()), id) {
		t.Fatal("narrow transcript lost the full session ID")
	}
}

func TestSessionHeaderRefreshesAfterClearAndResume(t *testing.T) {
	m := newTestAppModel(t)
	m.announcedSession = ""
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	original := m.orch.CurrentSessionID()
	header := ansi.Strip(m.renderHeaderBar())
	if !strings.Contains(header, original) {
		t.Fatalf("header lacks session ID: %s", header)
	}
	m.handleSubmit("/clear")
	fresh := m.orch.CurrentSessionID()
	text := ansi.Strip(m.renderTranscript())
	if fresh == original || strings.Contains(text, original) || !strings.Contains(text, "--resume "+fresh) {
		t.Fatalf("clear hint = %s", text)
	}
	header = ansi.Strip(m.renderHeaderBar())
	if !strings.Contains(header, fresh) || strings.Contains(header, original) {
		t.Fatalf("header cache retained old session: %s", header)
	}
	m.handleSubmit("/resume " + original)
	header = ansi.Strip(m.renderHeaderBar())
	if !strings.Contains(header, original) || strings.Contains(header, fresh) {
		t.Fatalf("resumed header = %s", header)
	}
	if m.announcedSession != original {
		t.Fatal("resumed session was not announced")
	}
}

func TestSessionPickerChangesAreAnnouncedOnHeartbeat(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	id, err := m.orch.ResetSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.Update(operationalSnapshotMsg{})
	if !strings.Contains(ansi.Strip(m.renderTranscript()), "--resume "+id) {
		t.Fatal("asynchronous session change omitted resume command")
	}
	count := len(m.blocks)
	m.Update(operationalSnapshotMsg{})
	if len(m.blocks) != count {
		t.Fatal("heartbeat duplicated resume hint")
	}
}

func TestSessionHeaderFitsSmallTerminal(t *testing.T) {
	m := newTestAppModel(t)
	for _, width := range []int{20, 30, 40, 80, 120} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 14})
		header := ansi.Strip(m.renderHeaderBar())
		if lipgloss.Width(header) > width || strings.Contains(header, "\n") {
			t.Fatalf("width %d header overflowed: %s", width, header)
		}
		if width >= 40 && !strings.Contains(header, "session ") {
			t.Fatalf("width %d omitted session indicator: %s", width, header)
		}
	}
}

func TestResumeHintIsSafeToCopy(t *testing.T) {
	if sessionResumeHint("") != "" {
		t.Fatal("empty session produced a command")
	}
	for _, id := range []string{"sess_abcd", "custom session; echo unsafe", "session'quoted"} {
		hint := sessionResumeHint(id)
		if !strings.Contains(hint, "Session: "+id) || !strings.Contains(hint, "Resume: chronos-code --resume ") {
			t.Fatalf("hint = %s", hint)
		}
		if id != "sess_abcd" && !strings.Contains(hint, "--resume '") {
			t.Fatalf("unsafe session ID was not quoted: %s", hint)
		}
	}
	hint := sessionResumeHint("bad\x1b[31m\nID")
	if strings.ContainsRune(hint, '\x1b') || strings.Contains(hint, "--resume") || !strings.Contains(hint, "/session list") {
		t.Fatalf("control characters reached terminal: %q", hint)
	}
}

type failingSessionHintWriter struct{}

func (failingSessionHintWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestSessionResumeHintPrintedAfterExit(t *testing.T) {
	var output strings.Builder
	if err := writeSessionResumeHint(&output, "sess_exit"); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "Session: sess_exit") || !strings.Contains(got, "chronos-code --resume sess_exit") {
		t.Fatalf("exit hint = %s", got)
	}
	if err := writeSessionResumeHint(failingSessionHintWriter{}, "sess_exit"); err == nil {
		t.Fatal("write error was lost")
	}
}
