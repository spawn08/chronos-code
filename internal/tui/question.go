package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/spawn08/chronos-code/internal/orchestrator"
)

type questionDialog struct {
	questions []orchestrator.UserQuestion
	answers   []orchestrator.UserAnswer
	index     int
	cursor    int
	selected  map[int]bool
	input     textinput.Model
	err       string
	result    orchestrator.UserQuestionResult
}

func newQuestionDialog(questions []orchestrator.UserQuestion) *questionDialog {
	d := &questionDialog{questions: questions}
	d.reset()
	return d
}

func (d *questionDialog) reset() {
	d.cursor = 0
	d.selected = make(map[int]bool)
	d.err = ""
	d.input = textinput.New()
	d.input.Placeholder = "Type your answer"
	d.input.CharLimit = 4096
	d.input.SetVirtualCursor(true)
	q := d.questions[d.index]
	for i, option := range q.Options {
		if option == q.Recommended {
			d.cursor = i
		}
	}
	if len(q.Options) == 0 {
		d.input.Focus()
	}
}

func (b *approvalBridge) userQuestion(ctx context.Context, questions []orchestrator.UserQuestion) (orchestrator.UserQuestionResult, error) {
	if err := b.acquire(ctx); err != nil {
		return orchestrator.UserQuestionResult{}, err
	}
	defer b.release()
	d := newQuestionDialog(questions)
	resp := make(chan approvalDecision, 1)
	b.p.Send(approvalRequestMsg{toolName: orchestrator.AskUserQuestionToolName, resp: resp, ctx: ctx, question: d})
	select {
	case <-resp:
		return d.result, nil
	case <-ctx.Done():
		b.p.Send(approvalClosedMsg{resp: resp})
		return orchestrator.UserQuestionResult{}, ctx.Err()
	}
}

func (d *questionDialog) custom() bool { return d.cursor == len(d.questions[d.index].Options) }

func (d *questionDialog) move(delta int) {
	count := len(d.questions[d.index].Options) + 1
	d.cursor = (d.cursor + delta + count) % count
	if d.custom() {
		d.input.Focus()
	} else {
		d.input.Blur()
	}
	d.err = ""
}

func (d *questionDialog) paste(text string) {
	if d.custom() {
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r", " "), "\n", " ")
		d.input, _ = d.input.Update(tea.PasteMsg{Content: text})
		d.err = ""
	}
}

func (d *questionDialog) submit() bool {
	q := d.questions[d.index]
	answer := orchestrator.UserAnswer{Question: q.Question}
	if q.MultiSelect {
		for i, option := range q.Options {
			if d.selected[i] {
				answer.Choices = append(answer.Choices, option)
			}
		}
		answer.Text = strings.TrimSpace(d.input.Value())
	} else if d.custom() {
		answer.Text = strings.TrimSpace(d.input.Value())
	} else {
		answer.Choices = []string{q.Options[d.cursor]}
	}
	if len(answer.Choices) == 0 && answer.Text == "" {
		d.err = "Select an option or type an answer."
		return false
	}
	d.answers = append(d.answers, answer)
	if d.index+1 < len(d.questions) {
		d.index++
		d.reset()
		return false
	}
	d.result = orchestrator.UserQuestionResult{Status: "answered", Answers: d.answers}
	return true
}

func (m *appModel) handleQuestionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	d := m.approval.question
	var cmd tea.Cmd
	done := false
	switch msg.String() {
	case "esc":
		d.result = orchestrator.UserQuestionResult{Status: "cancelled", Message: "The user cancelled the questions. Stop and wait for their feedback."}
		done = true
	case "up", "shift+tab":
		d.move(-1)
	case "down", "tab":
		d.move(1)
	case "enter":
		done = d.submit()
	case "space":
		if d.questions[d.index].MultiSelect && !d.custom() {
			d.selected[d.cursor] = !d.selected[d.cursor]
			d.err = ""
		} else if d.custom() {
			d.input, cmd = d.input.Update(msg)
		}
	default:
		if d.custom() {
			d.input, cmd = d.input.Update(msg)
			d.err = ""
		}
	}
	if done {
		// Publish the result before waking the blocked tool handler.
		m.approval.resp <- approvalDecision{}
		m.approval = nil
	}
	m.resizeViewport()
	return m, cmd
}

func (m *appModel) renderQuestionModal() string {
	d := m.approval.question
	q := d.questions[d.index]
	width := max(1, m.width-inputBoxBorderWidth-inputBoxPaddingWidth)
	d.input.SetWidth(max(1, width-2))
	header := truncateToWidth(fmt.Sprintf("Question %d/%d", d.index+1, len(d.questions)), width)
	lines := strings.Split(truncateApprovalDetails(ansi.Hardwrap(q.Question, width, true), 3), "\n")
	focus := len(lines) + d.cursor
	for i, option := range q.Options {
		prefix := "  "
		if i == d.cursor {
			prefix = "> "
		}
		if q.MultiSelect {
			mark := "[ ] "
			if d.selected[i] {
				mark = "[x] "
			}
			prefix += mark
		}
		if option == q.Recommended {
			option += " (Recommended)"
		}
		line := truncateToWidth(prefix+option, width)
		if i == d.cursor {
			line = styleAgentName.Render(line)
		}
		lines = append(lines, line)
	}
	custom := "  Other: " + d.input.Value()
	if d.custom() {
		custom = "> " + d.input.View()
	}
	lines = append(lines, truncateToWidth(custom, width))
	if d.err != "" {
		lines = append(lines, styleError.Render(truncateToWidth(d.err, width)))
	}
	// Keep the focused answer visible while leaving room for terminal chrome and transcript.
	budget := max(2, min(14, m.height-10))
	start := max(0, min(focus-budget+1, len(lines)-budget))
	if d.err != "" {
		start = max(start, len(lines)-budget)
	}
	end := min(len(lines), start+budget)
	body := strings.Join(lines[start:end], "\n")
	help := "↑↓/tab move · enter submit · esc cancel"
	if d.index+1 < len(d.questions) {
		help = "↑↓/tab move · enter next · esc cancel"
	}
	if q.MultiSelect {
		help = "↑↓/tab move · space select · enter submit · esc cancel"
	}
	// Short hints remain readable in narrow terminals.
	if width < 54 {
		help = "↑↓/tab · enter · esc cancel"
		if q.MultiSelect {
			help = "space select · enter · esc cancel"
		}
	}
	if width < 30 {
		help = "enter · esc cancel"
	}
	return styleApprovalModal.Width(max(1, m.width-inputBoxBorderWidth)).Render(styleHeader.Render(header) + "\n" + body + "\n" + styleDim.Render(truncateToWidth(help, width)))
}
