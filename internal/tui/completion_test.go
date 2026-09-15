package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spawn08/chronos/engine/model"
)

func TestStreamCompletionRetainsFinalTextAndReportsTruncation(t *testing.T) {
	for _, reason := range []model.StopReason{model.StopReasonEnd, model.StopReasonMaxTokens, model.StopReasonFilter} {
		t.Run(string(reason), func(t *testing.T) {
			m := newTestAppModel(t)
			m.sending = true
			ch := make(chan *model.ChatResponse)
			m.handleStreamDelta(streamDeltaMsg{ctx: m.ctx, ch: ch, resp: &model.ChatResponse{Content: "partial ", Delta: true}})
			m.handleStreamDelta(streamDeltaMsg{ctx: m.ctx, ch: ch, resp: &model.ChatResponse{
				Content: "partial ending", StopReason: reason, Usage: model.Usage{CompletionTokens: 12},
			}})
			if !m.sending {
				t.Fatal("final metadata must be drained before finalizing the turn")
			}
			m.Update(streamDoneMsg{})
			if m.sending || m.lastAssistantText != "partial ending" || m.lastKnownUsage.CompletionTokens != 12 {
				t.Fatalf("completion lost text/usage: sending=%v text=%q usage=%+v", m.sending, m.lastAssistantText, m.lastKnownUsage)
			}
			if reason == model.StopReasonEnd {
				if m.lastTurnErr != nil {
					t.Fatal(m.lastTurnErr)
				}
			} else if m.lastTurnErr == nil || !strings.Contains(strings.Join(m.blocks, "\n"), "response incomplete") {
				t.Fatalf("truncation was not visible: %v", m.lastTurnErr)
			}
		})
	}
}

func TestStreamEarlyCloseAndErrorPreservePartialResponse(t *testing.T) {
	for _, failure := range []error{nil, fmt.Errorf("connection lost")} {
		m := newTestAppModel(t)
		m.sending = true
		m.lastAssistantText = "old answer"
		m.handleStreamDelta(streamDeltaMsg{ctx: m.ctx, resp: nil})
		m.handleStreamDelta(streamDeltaMsg{ctx: m.ctx, resp: &model.ChatResponse{Content: "new partial answer", Delta: true, Err: failure}})
		if failure == nil {
			m.Update(streamDoneMsg{})
			if !errors.Is(m.lastTurnErr, io.ErrUnexpectedEOF) {
				t.Fatalf("early close error = %v", m.lastTurnErr)
			}
		} else if !errors.Is(m.lastTurnErr, failure) {
			t.Fatalf("stream error = %v", m.lastTurnErr)
		}
		if m.lastAssistantText != "new partial answer" {
			t.Fatalf("copy retained wrong answer: %q", m.lastAssistantText)
		}
	}
}

func TestBlockingResponseReportsOutputLimit(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	m.Update(chatDoneMsg{resp: &model.ChatResponse{Content: "partial answer", StopReason: model.StopReasonMaxTokens}})
	if m.lastTurnErr == nil || !strings.Contains(m.lastTurnErr.Error(), "output token limit") || m.lastAssistantText != "partial answer" {
		t.Fatalf("blocking completion = %q, %v", m.lastAssistantText, m.lastTurnErr)
	}
}

func TestClosedStreamRetainsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan *model.ChatResponse)
	close(ch)
	cancel()
	msg := listenStream(ctx, 7, ch)().(streamDoneMsg)
	if msg.turnID != 7 || !errors.Is(msg.err, context.Canceled) {
		t.Fatalf("completion = %+v", msg)
	}
}

func TestFollowingOutputSurvivesResizeAndComposerGrowth(t *testing.T) {
	m := newTestAppModel(t)
	m.followOutput = true
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	m.appendSystem(strings.Repeat("long transcript line\n", 100))
	m.refreshViewport()
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	if !m.viewport.AtBottom() {
		t.Fatal("resize hid the end of the response while following")
	}
	m.input.SetValue("one\ntwo\nthree")
	m.resizeViewport()
	if !m.viewport.AtBottom() {
		t.Fatal("growing composer hid the end of the response while following")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.followOutput {
		t.Fatal("page up did not detach output")
	}
	m.handleSubmit("next question")
	if !m.followOutput || !m.viewport.AtBottom() {
		t.Fatal("new question did not resume following output")
	}
	m.finalizeTurn(context.Canceled)
}

func TestMultilineStatusCannotOverlapComposer(t *testing.T) {
	for _, width := range []int{40, 80, 180} {
		m := newTestAppModel(t)
		m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
		m.statusMsg = "provider failed:\nfirst line\r\nsecond line"
		if got := lipgloss.Height(m.renderStatusBar()); got != 1 {
			t.Fatalf("status height at width %d = %d", width, got)
		}
		if got := lipgloss.Height(m.View().Content); got > 12 {
			t.Fatalf("view height at width %d = %d", width, got)
		}
	}
}

func TestScrolledStatusKeepsResumeHintVisible(t *testing.T) {
	for _, width := range []int{40, 80, 180} {
		m := newTestAppModel(t)
		m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
		m.followOutput = false
		status := ansi.Strip(m.renderStatusBar())
		if !strings.Contains(status, "ctrl+end") {
			t.Fatalf("resume hint hidden at width %d: %s", width, status)
		}
	}
}
