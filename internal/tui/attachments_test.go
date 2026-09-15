package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spawn08/chronos/engine/model"
)

func TestRawInputArtifactPreservesEntireInstruction(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("CHRONOS_CODE_DATA_HOME", home)
	message := "First instruction.\n" + strings.Repeat("raw data 世界\n", 10000) + "\nFinal instruction: do not edit."
	input, err := prepareInput(context.Background(), root, message, nil, maxInputBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Message) > maxInputBytes || !strings.Contains(input.Message, "retrieve the complete request, including its instructions") {
		t.Fatalf("unsafe raw-input replacement: %s", input.Message)
	}
	paths, _ := filepath.Glob(filepath.Join(home, "projects", "*", "artifacts", "request-*.txt"))
	if len(paths) != 1 {
		t.Fatalf("request artifacts = %v", paths)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil || string(data) != message {
		t.Fatalf("raw input lost: %v", err)
	}
	info, _ := os.Stat(paths[0])
	if info.Mode().Perm() != 0o600 || !strings.Contains(input.Message, paths[0]) {
		t.Fatalf("artifact mode/reference = %v / %s", info.Mode(), input.Message)
	}
}

func TestRawInputArtifactFailureDoesNotExecuteBulkInput(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(home, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHRONOS_CODE_DATA_HOME", home)
	input, err := prepareInput(context.Background(), root, strings.Repeat("instruction", 1000), nil, maxInputBytes)
	if err == nil || input.Message != "" {
		t.Fatalf("storage failure must stop execution, got %v / %d bytes", err, len(input.Message))
	}
}

func TestLargePasteSurvivesComposerLimitWithSurroundingInstructions(t *testing.T) {
	m := &appModel{input: newComposer()}
	m.input.SetValue("Before: ")
	m.input.CursorEnd()
	raw := strings.Repeat("\tlarge raw line 世界\n", 1000)
	m.insertPaste(raw)
	m.input.InsertString(" After: preserve this instruction.")
	if len(m.input.Value()) > 200 {
		t.Fatal("large paste passed through textarea instead of a lossless marker")
	}
	got := m.expandPastes(m.input.Value())
	if got != "Before: "+raw+" After: preserve this instruction." {
		t.Fatal("paste or surrounding instructions changed")
	}
}

type attachmentProvider struct {
	requests chan *model.ChatRequest
	modelID  string
}

func (p *attachmentProvider) Name() string { return "openai" }
func (p *attachmentProvider) Model() string {
	if p.modelID != "" {
		return p.modelID
	}
	return "gpt-4o-mini"
}
func (p *attachmentProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.requests <- req
	return &model.ChatResponse{Role: model.RoleAssistant, Content: "done"}, nil
}
func (p *attachmentProvider) StreamChat(ctx context.Context, req *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	ch := make(chan *model.ChatResponse, 1)
	ch <- resp
	close(ch)
	return ch, err
}

func TestSendCmdPreparesAtExecutionAndReturnsReceipt(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			m := newTestAppModel(t)
			m.stream = stream
			p := &attachmentProvider{requests: make(chan *model.ChatRequest, 2)}
			m.orch.ActiveAgent().Model = p
			root := m.workspaceRoot()
			path := filepath.Join(root, "source")
			if err := os.WriteFile(path, []byte("before command"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := m.sendCmd(context.Background(), 7, "inspect @source")
			if err := os.WriteFile(path, []byte("after command created"), 0o600); err != nil {
				t.Fatal(err)
			}
			msg := cmd()
			var receipt string
			switch v := msg.(type) {
			case chatDoneMsg:
				if v.err != nil {
					t.Fatal(v.err)
				}
				receipt = v.attachments
			case streamStartedMsg:
				receipt = v.attachments
				for range v.ch {
				}
			default:
				t.Fatalf("unexpected result %T", msg)
			}
			select {
			case req := <-p.requests:
				var text string
				for _, message := range req.Messages {
					text += message.Content
				}
				if !strings.Contains(text, "after command created") || strings.Contains(text, "before command") {
					t.Fatalf("attachment read before command ran: %s", text)
				}
			default:
				t.Fatal("provider was not called")
			}
			if !strings.Contains(receipt, `"source": included:`) {
				t.Fatalf("receipt missing: %s", receipt)
			}
		})
	}
}

func TestCanceledSendDoesNotReadOrExecute(t *testing.T) {
	m := newTestAppModel(t)
	p := &attachmentProvider{requests: make(chan *model.ChatRequest, 1)}
	m.orch.ActiveAgent().Model = p
	ctx, cancel := context.WithCancel(context.Background())
	cmd := m.sendCmd(ctx, 3, strings.Repeat("bulk paste", 10000))
	cancel()
	msg := cmd().(chatDoneMsg)
	if msg.turnID != 3 || !errors.Is(msg.err, context.Canceled) || len(p.requests) != 0 {
		t.Fatalf("cancel failed: %+v", msg)
	}
	paths, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".chronos-code", "projects", "*", "artifacts", "*"))
	if len(paths) != 0 {
		t.Fatalf("canceled command created artifacts: %v", paths)
	}
}

func TestSendCmdUsesConfiguredContextBudget(t *testing.T) {
	for _, tc := range []struct {
		modelID string
		window  int
	}{
		{"custom-deployment", 4096}, // 1024-byte initial input cap.
		{"gpt-4", 128000},           // Known 8192-token model clamps config: 2048 bytes.
	} {
		t.Run(tc.modelID, func(t *testing.T) {
			m := newTestAppModel(t)
			p := &attachmentProvider{requests: make(chan *model.ChatRequest, 1), modelID: tc.modelID}
			m.orch.ActiveAgent().Model = p
			m.orch.ActiveAgent().ContextCfg.MaxContextTokens = tc.window
			message := strings.Repeat("preserve this instruction. ", 60)
			msg := m.sendCmd(context.Background(), 1, message)().(chatDoneMsg)
			if msg.err != nil {
				t.Fatal(msg.err)
			}
			if !strings.Contains(msg.attachments, "Full user request stored losslessly:") {
				t.Fatalf("configured small-model budget was ignored: %s", msg.attachments)
			}
			req := <-p.requests
			for _, item := range req.Messages {
				if item.Role == model.RoleUser && strings.Contains(item.Content, message) {
					t.Fatal("oversized raw request reached the model")
				}
			}
		})
	}
}

func TestSubmitEchoesOnlyOriginalInput(t *testing.T) {
	m := newTestAppModel(t)
	if err := os.WriteFile(filepath.Join(m.workspaceRoot(), "source"), []byte("private attachment body"), 0o600); err != nil {
		t.Fatal(err)
	}
	message := "inspect @source"
	_, cmd := m.handleSubmit(message)
	defer m.turnCancel()
	if m.stopActivity != nil {
		defer m.stopActivity()
	}
	if cmd == nil || m.activeRequest != message {
		t.Fatalf("submit enriched the request on Update: %q", m.activeRequest)
	}
	transcript := strings.Join(m.blocks, "\n")
	if !strings.Contains(transcript, message) || strings.Contains(transcript, "private attachment body") || strings.Contains(transcript, "Attachment receipt") {
		t.Fatalf("user echo was expanded: %s", transcript)
	}
}

func TestCanceledStreamStartDoesNotReviveTurn(t *testing.T) {
	m := newTestAppModel(t)
	m.sending, m.turnID = true, 3
	m.turnCtx, m.turnCancel = context.WithCancel(context.Background())
	ctx := m.turnCtx
	m.interruptTurn()
	_, cmd := m.Update(streamStartedMsg{turnID: 3, ctx: ctx, attachments: "source: omitted"})
	if cmd != nil || m.sending || !m.lastTurnInterrupted {
		t.Fatal("late stream start revived canceled turn")
	}
}

func TestAttachmentMetadataIgnoresStaleAndSettledTurns(t *testing.T) {
	m := newTestAppModel(t)
	m.turnID, m.sending = 9, true
	for _, msg := range []tea.Msg{
		streamStartedMsg{turnID: 8, attachments: "STALE", ctx: context.Background()},
		chatDoneMsg{turnID: 8, attachments: "STALE"},
		maintenanceDoneMsg{turnID: 8, text: "STALE"},
	} {
		_, cmd := m.Update(msg)
		if cmd != nil || len(m.activeTurnItems) != 0 || !m.sending {
			t.Fatalf("stale %T affected active turn", msg)
		}
	}
	m.sending = false
	_, cmd := m.Update(chatDoneMsg{turnID: 9, attachments: "SETTLED"})
	if cmd != nil || strings.Contains(m.renderTranscript(), "SETTLED") {
		t.Fatal("settled turn accepted late receipt")
	}
}

func TestAttachmentReceiptVisibleWithCollapsedToolsAtAllWidths(t *testing.T) {
	for _, width := range []int{40, 80, 120} {
		m := &appModel{}
		m.viewport.SetWidth(width)
		m.captureAttachmentReceipt("Attachment receipt:\n- source: empty (0 bytes)\n- missing: error: missing file\n- " + strings.Repeat("long-path/", 20) + ": omitted: aggregate budget")
		m.appendTurnActivity("context selected")
		m.appendTurnActivity("file_read · done")
		got := m.renderItemList(m.activeTurnItems)
		for _, want := range []string{"empty (0 bytes)", "missing file", "aggregate budget"} {
			if !strings.Contains(got, want) {
				t.Fatalf("width %d hid receipt %q: %s", width, want, got)
			}
		}
		if lipgloss.Width(got) > width {
			t.Fatalf("receipt overflows %d columns: width=%d", width, lipgloss.Width(got))
		}
	}
}

func TestMaintenanceSynchronousCallbackDoesNotBlockUpdate(t *testing.T) {
	m := newTestAppModel(t)
	requests := make(chan approvalRequestMsg, 1)
	called := false
	cmd := m.maintenanceCmd("connecting", func(ctx context.Context) (string, error) {
		called = true
		response := make(chan approvalDecision, 1)
		requests <- approvalRequestMsg{toolName: "mcp", resp: response}
		select {
		case decision := <-response:
			if !decision.allow {
				return "", fmt.Errorf("denied")
			}
			return "connected", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	if called || cmd == nil {
		t.Fatal("operation ran on Update")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case request := <-requests:
		m.Update(request)
		m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	case <-time.After(5 * time.Second):
		t.Fatal("callback not reached")
	}
	select {
	case msg := <-done:
		m.Update(msg)
		if m.sending || !strings.Contains(m.renderTranscript(), "connected") {
			t.Fatal("maintenance did not settle")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("synchronous callback deadlocked")
	}
}

func TestMaintenanceCancellationAndSlashCommandsAreDeferred(t *testing.T) {
	for _, line := range []string{"/compact", "/mcp connect unknown"} {
		t.Run(line, func(t *testing.T) {
			m := newTestAppModel(t)
			_, cmd := m.handleSlashCommand(line)
			if cmd == nil || !m.sending {
				t.Fatal("operation was not deferred")
			}
			m.interruptTurn()
			msg := cmd().(maintenanceDoneMsg)
			if !errors.Is(msg.err, context.Canceled) {
				t.Fatalf("operation ran after cancel: %+v", msg)
			}
			m.Update(msg)
			if m.sending {
				t.Fatal("canceled operation did not settle")
			}
		})
	}
}

func TestPartialActionsAreNeverRetried(t *testing.T) {
	for _, failure := range []error{
		fmt.Errorf("token budget exceeded for session test"),
		fmt.Errorf("maximum context length exceeded"),
	} {
		m := newTestAppModel(t)
		m.sending, m.turnID, m.activeRequest = true, 4, "perform this edit"
		m.appendTurnActivity("file_write · done")
		_, cmd := m.handleStreamDelta(streamDeltaMsg{turnID: 4, resp: &model.ChatResponse{Err: failure}})
		if cmd != nil || m.sending || m.turnID != 4 || m.budgetRetried {
			t.Fatalf("whole task was retried: %v", failure)
		}
		if !strings.Contains(m.renderTranscript(), "file_write") {
			t.Fatal("partial action receipt lost")
		}
	}
}
