package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos/engine/model"
)

func showTestQuestion(t *testing.T, questions []orchestrator.UserQuestion) (*appModel, *questionDialog, chan approvalDecision) {
	t.Helper()
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	d := newQuestionDialog(questions)
	resp := make(chan approvalDecision, 1)
	m.Update(approvalRequestMsg{toolName: orchestrator.AskUserQuestionToolName, question: d, resp: resp})
	return m, d, resp
}

func TestQuestionDialogSingleAndCustomAnswers(t *testing.T) {
	m, d, resp := showTestQuestion(t, []orchestrator.UserQuestion{
		{Question: "Database?", Options: []string{"SQLite", "Postgres"}, Recommended: "Postgres"},
		{Question: "Project name?"},
	})
	if view := ansi.Strip(m.renderApprovalModal()); !strings.Contains(view, "> Postgres (Recommended)") {
		t.Fatalf("recommended choice missing: %s", view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(resp) != 0 || d.index != 1 {
		t.Fatal("first question prematurely completed the tool")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if d.err == "" || len(resp) != 0 {
		t.Fatal("empty custom answer accepted")
	}
	m.Update(tea.KeyPressMsg{Code: '名', Text: "名前"})
	m.Update(tea.PasteMsg{Content: " project\nname"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.approval != nil || len(resp) != 1 || d.result.Status != "answered" {
		t.Fatal("questions did not complete")
	}
	want := []orchestrator.UserAnswer{{Question: "Database?", Choices: []string{"Postgres"}}, {Question: "Project name?", Text: "名前 project name"}}
	if !reflect.DeepEqual(d.result.Answers, want) {
		t.Fatalf("answers = %#v", d.result.Answers)
	}
}

func TestQuestionDialogMultiSelect(t *testing.T) {
	m, d, resp := showTestQuestion(t, []orchestrator.UserQuestion{{Question: "Platforms?", Options: []string{"Linux", "macOS", "Windows"}, MultiSelect: true}})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if d.err == "" || len(resp) != 0 {
		t.Fatal("empty multi-selection accepted")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}) // toggle macOS off
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m.Update(tea.PasteMsg{Content: "BSD"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if d.result.Status != "answered" || !reflect.DeepEqual(d.result.Answers[0].Choices, []string{"Linux"}) || d.result.Answers[0].Text != "BSD" {
		t.Fatalf("result = %#v", d.result)
	}
}

func TestQuestionDialogCancellationAndInputIsolation(t *testing.T) {
	m, d, resp := showTestQuestion(t, []orchestrator.UserQuestion{{Question: "Choose", Options: []string{"a", "b"}}})
	m.input.SetValue("existing composer")
	m.Update(tea.PasteMsg{Content: "ignored on choices"})
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.input.Value() != "existing composer" || d.input.Value() != "" {
		t.Fatal("question input leaked into composer")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.approval != nil || len(resp) != 1 || d.result.Status != "cancelled" || len(d.result.Answers) != 0 {
		t.Fatalf("cancel result = %#v", d.result)
	}
}

func TestQuestionDialogResponsiveAndFocusedChoice(t *testing.T) {
	for _, size := range []struct{ width, height int }{{100, 30}, {40, 14}, {24, 12}} {
		t.Run(strings.Repeat("w", size.width), func(t *testing.T) {
			options := []string{"one", "two", "three", "four", "five", "six", "seven", "eight"}
			m, d, _ := showTestQuestion(t, []orchestrator.UserQuestion{{Question: strings.Repeat("A long question 世界 ", 20), Options: options}})
			m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			for i := 0; i < len(options); i++ {
				m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			}
			view := ansi.Strip(m.renderApprovalModal())
			if d.cursor != len(options) || !strings.Contains(view, "> ") {
				t.Fatalf("focused custom answer hidden: %s", view)
			}
			if lipgloss.Width(view) > size.width || lipgloss.Height(view) > size.height-3 {
				t.Fatalf("oversized modal (%dx%d): %s", lipgloss.Width(view), lipgloss.Height(view), view)
			}
			if m.View().MouseMode != tea.MouseModeCellMotion {
				t.Fatal("question changed default mouse capture")
			}
		})
	}
}

func TestQuestionPromptStaleCancellation(t *testing.T) {
	m, _, resp := showTestQuestion(t, []orchestrator.UserQuestion{{Question: "Still active?"}})
	m.Update(approvalClosedMsg{resp: make(chan approvalDecision, 1)})
	if m.approval == nil {
		t.Fatal("stale cancellation closed active dialog")
	}
	m.Update(approvalClosedMsg{resp: resp})
	if m.approval != nil {
		t.Fatal("cancelled prompt remained visible")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Update(approvalRequestMsg{ctx: ctx, resp: resp})
	if m.approval != nil {
		t.Fatal("cancelled request opened dialog")
	}
}

type questionTestProgram struct {
	app    *appModel
	events chan tea.Msg
}

func (m *questionTestProgram) Init() tea.Cmd  { return nil }
func (m *questionTestProgram) View() tea.View { return tea.View{} }
func (m *questionTestProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := m.app.Update(msg)
	switch msg.(type) {
	case approvalRequestMsg, approvalClosedMsg:
		m.events <- msg
	}
	return m, cmd
}

func nextQuestionEvent(t *testing.T, events <-chan tea.Msg) tea.Msg {
	t.Helper()
	select {
	case msg := <-events:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for prompt")
		return nil
	}
}

func TestQuestionBridgeRoundTripAndApprovalSerialization(t *testing.T) {
	m := &questionTestProgram{app: newTestAppModel(t), events: make(chan tea.Msg, 8)}
	p := tea.NewProgram(m, tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer())
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	t.Cleanup(func() {
		p.Quit()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("program: %v", err)
			}
		case <-time.After(5 * time.Second):
			p.Kill()
			t.Error("program did not exit")
		}
	})
	bridge := newApprovalBridge(p)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	answers := make(chan orchestrator.UserQuestionResult, 1)
	go func() {
		result, err := bridge.userQuestion(ctx, []orchestrator.UserQuestion{{Question: "Database?", Options: []string{"SQLite", "Postgres"}}})
		if err != nil {
			answers <- orchestrator.UserQuestionResult{Message: err.Error()}
			return
		}
		answers <- result
	}()
	if msg := nextQuestionEvent(t, m.events).(approvalRequestMsg); msg.question == nil {
		t.Fatal("question did not reach program")
	}
	approval := make(chan bool, 1)
	go func() { ok, _ := bridge.toolApproval(ctx, "shell", nil); approval <- ok }()
	// A waiting prompt can be cancelled independently of the active dialog.
	waiting, stopWaiting := context.WithCancel(ctx)
	cancelled := make(chan error, 1)
	go func() {
		_, err := bridge.userQuestion(waiting, []orchestrator.UserQuestion{{Question: "Cancelled waiter?"}})
		cancelled <- err
	}()
	stopWaiting()
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting cancellation: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("waiting prompt ignored cancellation")
	}
	select {
	case <-m.events:
		t.Fatal("approval overwrote active question")
	default:
	}
	p.Send(tea.KeyPressMsg{Code: tea.KeyDown})
	p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case result := <-answers:
		if result.Status != "answered" || result.Answers[0].Choices[0] != "Postgres" {
			t.Fatalf("answer = %#v", result)
		}
	case <-ctx.Done():
		t.Fatal("answer not delivered")
	}
	if msg := nextQuestionEvent(t, m.events).(approvalRequestMsg); msg.toolName != "shell" || msg.question != nil {
		t.Fatal("approval did not follow question")
	}
	p.Send(tea.KeyPressMsg{Code: 'y', Text: "y"})
	select {
	case ok := <-approval:
		if !ok {
			t.Fatal("approval declined")
		}
	case <-ctx.Done():
		t.Fatal("approval not delivered")
	}
}

func TestQuestionBridgeContextCancellation(t *testing.T) {
	m := &questionTestProgram{app: newTestAppModel(t), events: make(chan tea.Msg, 4)}
	p := tea.NewProgram(m, tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer())
	done := make(chan struct{})
	go func() { defer close(done); _, _ = p.Run() }()
	t.Cleanup(func() {
		p.Quit()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			p.Kill()
			t.Error("program did not exit")
		}
	})
	bridge := newApprovalBridge(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := bridge.userQuestion(ctx, []orchestrator.UserQuestion{{Question: "Pending?"}})
		result <- err
	}()
	nextQuestionEvent(t, m.events)
	cancel()
	if _, ok := nextQuestionEvent(t, m.events).(approvalClosedMsg); !ok {
		t.Fatal("cancel did not close prompt")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not release handler")
	}
}

// The scripted provider exercises the real agent loop without external credentials.
type questionFlowProvider struct{ received chan string }

func (p *questionFlowProvider) Name() string  { return "openai" }
func (p *questionFlowProvider) Model() string { return "gpt-4o-mini" }
func (p *questionFlowProvider) Chat(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	for _, message := range req.Messages {
		if message.Role == model.RoleTool && message.ToolCallID == "question-1" {
			select {
			case p.received <- message.Content:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &model.ChatResponse{Role: model.RoleAssistant, Content: "Your database choice is Postgres.", StopReason: model.StopReasonEnd, UsageKnown: true}, nil
		}
	}
	return &model.ChatResponse{Role: model.RoleAssistant, StopReason: model.StopReasonToolCall, UsageKnown: true, ToolCalls: []model.ToolCall{{ID: "question-1", Name: orchestrator.AskUserQuestionToolName, Arguments: `{"questions":[{"question":"Database?","options":["SQLite","Postgres"]}]}`}}}, nil
}
func (p *questionFlowProvider) StreamChat(ctx context.Context, req *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	response, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan *model.ChatResponse, 1)
	ch <- response
	close(ch)
	return ch, nil
}

func TestQuestionAgentContinuesInSameRun(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			m := &questionTestProgram{app: newTestAppModel(t), events: make(chan tea.Msg, 8)}
			m.app.stream = streaming
			provider := &questionFlowProvider{received: make(chan string, 2)}
			m.app.orch.ActiveAgent().Model = provider
			p := tea.NewProgram(m, tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer())
			done := make(chan struct{})
			go func() { defer close(done); _, _ = p.Run() }()
			t.Cleanup(func() {
				p.Quit()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					p.Kill()
					t.Error("program did not exit")
				}
			})
			bridge := newApprovalBridge(p)
			m.app.orch.SetUserQuestionHandler(bridge.userQuestion)
			installApprovalHandlers(m.app.orch, bridge.toolApproval)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := m.app.sendCmd(ctx, 1, "Ask me to choose a database and report my choice.")
			completed := make(chan tea.Msg, 1)
			go func() { completed <- cmd() }()
			var msg approvalRequestMsg
			if streaming {
				msg = nextQuestionEvent(t, m.events).(approvalRequestMsg)
			} else {
				select {
				case event := <-m.events:
					msg = event.(approvalRequestMsg)
				case early := <-completed:
					v := early.(chatDoneMsg)
					t.Fatalf("run completed before question: response=%#v, error=%v", v.resp, v.err)
				case <-ctx.Done():
					t.Fatal("question not opened")
				}
			}
			if msg.question == nil {
				t.Fatalf("unexpected permission prompt before question: %s", msg.toolName)
			}
			p.Send(tea.KeyPressMsg{Code: tea.KeyDown})
			p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
			select {
			case content := <-provider.received:
				if !strings.Contains(content, "Postgres") || !strings.Contains(content, "answered") {
					t.Fatalf("agent received %s", content)
				}
			case <-ctx.Done():
				t.Fatal("agent did not receive question answer")
			}
			select {
			case result := <-completed:
				switch v := result.(type) {
				case chatDoneMsg:
					if v.err != nil || v.resp == nil || v.resp.Content != "Your database choice is Postgres." {
						t.Fatalf("continuation = %#v / %v", v.resp, v.err)
					}
				case streamStartedMsg:
					var content string
					for response := range v.ch {
						if response.Err != nil {
							t.Fatal(response.Err)
						}
						content += response.Content
					}
					if content != "Your database choice is Postgres." {
						t.Fatalf("stream continuation = %q", content)
					}
				default:
					t.Fatalf("unexpected completion %T", result)
				}
			case <-ctx.Done():
				t.Fatal("agent run did not finish")
			}
		})
	}
}

func TestQuestionCustomPasteUsesCursorAndLimit(t *testing.T) {
	m, d, _ := showTestQuestion(t, []orchestrator.UserQuestion{{Question: "Name?"}})
	d.input.SetValue("ac")
	d.input.SetCursor(1)
	m.Update(tea.PasteMsg{Content: "b"})
	if d.input.Value() != "abc" {
		t.Fatalf("paste ignored cursor: %q", d.input.Value())
	}
	m.Update(tea.PasteMsg{Content: strings.Repeat("界", 5000)})
	if len([]rune(d.input.Value())) != d.input.CharLimit {
		t.Fatal("paste exceeded the answer length limit")
	}
}
