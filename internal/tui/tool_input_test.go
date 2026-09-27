package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	chronosstream "github.com/spawn08/chronos/engine/stream"
)

func toolInputEvent(agent, id, tool string, bytes int) activityMsg {
	return activityMsg{event: chronosstream.Event{Type: chronosstream.EventToolInput, Data: map[string]any{
		"agent": agent, "id": id, "tool": tool, "bytes": bytes,
	}}}
}

func activityLines(m *appModel) []string {
	var lines []string
	for _, item := range m.activeTurnItems {
		if item.kind == turnItemActivity {
			lines = append(lines, ansi.Strip(item.content))
		}
	}
	return lines
}

func TestToolInputShowsProgressThenBecomesToolLine(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	for _, bytes := range []int{0, 4096, 18432} {
		msg := toolInputEvent("coder", "call-1", "file_write", bytes)
		msg.ctx = m.ctx
		m.handleActivity(msg)
	}
	lines := activityLines(m)
	if len(lines) != 1 || !strings.Contains(lines[0], "@coder file_write · writing · 18.0 KiB") {
		t.Fatalf("streaming input lines = %q, want one updating writing line", lines)
	}

	m.handleActivity(activityMsg{ctx: m.ctx, event: chronosstream.Event{Type: chronosstream.EventToolCall, Data: map[string]any{
		"agent": "coder", "id": "call-1", "tool": "file_write", "args": map[string]any{"path": "big.css"},
	}}})
	lines = activityLines(m)
	if len(lines) != 1 || !strings.Contains(lines[0], "file_write · running") {
		t.Fatalf("after tool_call lines = %q, want the input line replaced by the running line", lines)
	}

	// A late progress event for a call that already started changes nothing.
	late := toolInputEvent("coder", "call-1", "file_write", 20000)
	late.ctx = m.ctx
	m.handleActivity(late)
	if got := activityLines(m); len(got) != 1 || !strings.Contains(got[0], "running") {
		t.Fatalf("late input event altered lines: %q", got)
	}

	m.handleActivity(activityMsg{ctx: m.ctx, event: chronosstream.Event{Type: chronosstream.EventToolResult, Data: map[string]any{
		"agent": "coder", "id": "call-1", "tool": "file_write", "result": "ok",
	}}})
	if got := activityLines(m); len(got) != 1 || !strings.Contains(got[0], "file_write · done") {
		t.Fatalf("after tool_result lines = %q", got)
	}
}

func TestToolInputNeverRunIsSettled(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	msg := toolInputEvent("coder", "call-1", "file_write", 90000)
	msg.ctx = m.ctx
	m.handleActivity(msg)
	other := toolInputEvent("tester", "call-9", "shell", 10)
	other.ctx = m.ctx
	m.handleActivity(other)

	// The reply was cut off: the same agent starts its next model round.
	m.handleActivity(activityMsg{ctx: m.ctx, event: chronosstream.Event{Type: chronosstream.EventModelCall, Data: map[string]any{
		"agent": "coder", "model": "anthropic",
	}}})
	got := strings.Join(activityLines(m), "\n")
	if !strings.Contains(got, "@coder file_write · not run: reply ended mid-call · 87.9 KiB") {
		t.Fatalf("cut-off call not settled: %s", got)
	}
	if !strings.Contains(got, "@tester shell · writing") {
		t.Fatalf("another agent's in-flight call was settled: %s", got)
	}

	m.finalizeTurn(nil)
	if got := ansi.Strip(m.renderTranscript()); strings.Contains(got, "· writing") || !strings.Contains(got, "@tester shell · not run") {
		t.Fatalf("finalized turn left a call in progress: %s", got)
	}
}
