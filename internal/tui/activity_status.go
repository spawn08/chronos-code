package tui

import (
	"fmt"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

func (m *appModel) workInProgress() bool {
	return m.sending || m.shellPending > 0
}

func (m *appModel) startSpinner() tea.Cmd {
	if m.spinnerRunning {
		return nil
	}
	if len(m.spin.Spinner.Frames) == 0 {
		m.spin = spinner.New(spinner.WithSpinner(spinner.MiniDot))
	}
	m.spinnerRunning = true
	return m.spin.Tick
}

func (m *appModel) activityStatus() string {
	marker, label := "●", "idle"
	if m.workInProgress() {
		label = "running"
		if len(m.spin.Spinner.Frames) > 0 {
			marker = m.spin.View()
		}
		switch {
		case m.turnInterrupted:
			label = "stopping"
		case !m.sending:
			label = "shell"
		case m.pendingSubagents > 0:
			label = fmt.Sprintf("running · %d agents", m.pendingSubagents)
		case m.pendingToolCalls > 0:
			label = fmt.Sprintf("running · %d tools", m.pendingToolCalls)
		}
		if !m.turnInterrupted && m.sending && m.workLabel != "" {
			label = truncateToWidth(m.workLabel, max(6, m.width-10))
		}
	}
	if m.approval != nil {
		marker, label = "?", "awaiting approval"
		if m.approval.question != nil {
			label = "awaiting answer"
		}
	}
	text := " " + marker + " " + label
	if m.workInProgress() && !m.busyStarted.IsZero() {
		seconds := max(0, int(time.Since(m.busyStarted).Seconds()))
		text += fmt.Sprintf(" · %ds", seconds)
	}
	return text
}
