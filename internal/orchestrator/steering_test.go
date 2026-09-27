package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/workspace"
)

func TestSteeringInboxDeliversOrReturnsEachMessageOnce(t *testing.T) {
	inbox := NewSteeringInbox()
	if inbox.Push("   ") {
		t.Fatal("blank input accepted")
	}
	inbox.Push("first")
	inbox.Push("second")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := inbox.DrainPendingInput(canceled); got != nil {
		t.Fatalf("drain on a canceled turn = %q, want input kept for the client", got)
	}

	got := inbox.DrainPendingInput(context.Background())
	if len(got) != 1 || !strings.Contains(got[0], steeringPreamble) || !strings.Contains(got[0], "first\n\nsecond") {
		t.Fatalf("drained = %q", got)
	}
	select {
	case <-inbox.Delivered():
	default:
		t.Fatal("delivery was not signaled")
	}
	if raw := inbox.TakeDelivered(); strings.Join(raw, "|") != "first|second" {
		t.Fatalf("delivered = %q", raw)
	}
	if again := inbox.DrainPendingInput(context.Background()); again != nil {
		t.Fatalf("second drain = %q, want nothing", again)
	}

	inbox.Push("late")
	if left := inbox.Close(); strings.Join(left, "|") != "late" {
		t.Fatalf("Close() = %q", left)
	}
	if inbox.Push("after close") || inbox.DrainPendingInput(context.Background()) != nil {
		t.Fatal("closed inbox still accepts or delivers input")
	}
}

// steeringProvider records each request and runs one tool round.
type steeringProvider struct {
	mu       sync.Mutex
	requests [][]model.Message
}

func (p *steeringProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, append([]model.Message(nil), req.Messages...))
	if len(p.requests) == 1 {
		return &model.ChatResponse{Role: model.RoleAssistant, StopReason: model.StopReasonToolCall,
			ToolCalls: []model.ToolCall{{ID: "call-1", Name: "probe", Arguments: `{}`}}}, nil
	}
	return &model.ChatResponse{Role: model.RoleAssistant, Content: "finished", StopReason: model.StopReasonEnd}, nil
}

func (p *steeringProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, nil
}
func (p *steeringProvider) Name() string  { return "steering" }
func (p *steeringProvider) Model() string { return "steering-model" }

func TestExecuteAddsSteeringInputToRunningTask(t *testing.T) {
	provider := &steeringProvider{}
	a := newExecutionTestAgent("coder", provider)
	inbox := NewSteeringInbox()
	a.Tools.Register(&tool.Definition{Name: "probe", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectRead}, Handler: func(context.Context, map[string]any) (any, error) {
		inbox.Push("also cover the empty input case") // typed while the tool runs
		return "ok", nil
	}})
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", workspace: &workspace.Info{Root: t.TempDir()}, cfg: &config.Config{}}

	result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "fix the parser", RequestedAgent: "coder", PendingInput: inbox})
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == nil || result.Response.Content != "finished" || len(provider.requests) != 2 {
		t.Fatalf("result = %+v, model calls = %d", result.Response, len(provider.requests))
	}
	follow := provider.requests[1]
	last := follow[len(follow)-1]
	if last.Role != model.RoleUser || !strings.Contains(last.Content, "<user_update>") || !strings.Contains(last.Content, "also cover the empty input case") {
		t.Fatalf("follow-up tail = %+v, want the steering input after the tool result", last)
	}
	if raw := inbox.TakeDelivered(); len(raw) != 1 || inbox.Close() != nil {
		t.Fatalf("delivered = %q, want the input consumed by the running task", raw)
	}
}
