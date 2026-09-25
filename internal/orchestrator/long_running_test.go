package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/harness"
	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/workspace"
)

func governorRound(name, args string) agent.ToolRound {
	return agent.ToolRound{ToolCalls: []model.ToolCall{{ID: "call", Name: name, Arguments: args}}}
}

func testGovernor(t *testing.T, window workWindow, runtime *taskRuntime) *windowGovernor {
	t.Helper()
	return newWindowGovernor(&longRunningPolicy{window: window, noProgressWindows: 2}, runtime, time.Now)
}

func TestWindowGovernorPausesAfterConsecutiveWindowsWithoutProgress(t *testing.T) {
	g := testGovernor(t, workWindow{rounds: 2}, nil)
	for round := 1; round <= 6; round++ {
		action, err := g.AfterToolRound(context.Background(), governorRound("file_read", `{"path":"a.go"}`))
		if err != nil {
			t.Fatal(err)
		}
		// Window 1 (rounds 1-2) issues a new call; windows 2 and 3 only repeat it.
		if action.Stop != (round == 6) {
			t.Fatalf("round %d stop = %v", round, action.Stop)
		}
		if action.Stop && !strings.Contains(action.Message, "without progress") {
			t.Fatalf("pause message = %q", action.Message)
		}
	}
}

func TestWindowGovernorCountsNewFileContentAsProgress(t *testing.T) {
	runtime, err := newTaskRuntime("task", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := testGovernor(t, workWindow{rounds: 2}, runtime)
	for round := 1; round <= 40; round++ {
		if round%2 == 0 {
			if _, err := runtime.recordWrite("main.go", fmt.Sprintf("hash-%d", round), 1, execution.ProvenanceRuntime, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		action, err := g.AfterToolRound(context.Background(), governorRound("file_write", `{"path":"main.go"}`))
		if err != nil || action.Stop {
			t.Fatalf("round %d = %+v, %v; new content every window is progress", round, action, err)
		}
	}
}

func TestWindowGovernorIgnoresEvidenceFromBeforeTheRun(t *testing.T) {
	runtime, err := newTaskRuntime("task", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exit := 1
	now := time.Now()
	if _, err := runtime.recordCommand("go test ./...", execution.CommandTest, &exit, execution.TerminalExited, nil, execution.ProvenanceRuntime, now, now); err != nil {
		t.Fatal(err)
	}
	g := testGovernor(t, workWindow{rounds: 1}, runtime)
	stopped := 0
	for round := 1; round <= 3; round++ {
		// Re-running the same failing check is not new evidence.
		if _, err := runtime.recordCommand("go test ./...", execution.CommandTest, &exit, execution.TerminalExited, nil, execution.ProvenanceRuntime, now, now); err != nil {
			t.Fatal(err)
		}
		action, _ := g.AfterToolRound(context.Background(), governorRound("shell", `{"command":"go test ./..."}`))
		if action.Stop {
			stopped = round
			break
		}
	}
	if stopped != 3 {
		t.Fatalf("stopped at round %d, want 3 (first window has a new call, then two idle windows)", stopped)
	}
}

func TestWindowGovernorExplorationPastTheOldIterationCap(t *testing.T) {
	g := testGovernor(t, workWindow{rounds: 10, calls: 20}, nil)
	for round := 1; round <= 300; round++ {
		action, err := g.AfterToolRound(context.Background(), governorRound("file_read", fmt.Sprintf(`{"path":"pkg/%d.go"}`, round)))
		if err != nil || action.Stop {
			t.Fatalf("round %d = %+v, %v; reading new files is progress", round, action, err)
		}
	}
}

func TestWindowGovernorRenewsTheSessionTokenBudget(t *testing.T) {
	tracker := budget.NewTracker(100, 0)
	ctx := storage.WithSession(context.Background(), "session-1")
	if err := tracker.After(ctx, &hooks.Event{Type: hooks.EventModelCallAfter, Output: &model.ChatResponse{Usage: model.Usage{CompletionTokens: 95}}}); err != nil {
		t.Fatal(err)
	}
	g := newWindowGovernor(&longRunningPolicy{noProgressWindows: 2, budget: tracker}, nil, time.Now)
	action, err := g.AfterToolRound(ctx, governorRound("file_read", `{"path":"a.go"}`))
	if err != nil || action.Stop {
		t.Fatalf("action = %+v, err = %v", action, err)
	}
	if used := tracker.Used("session-1"); used != 0 {
		t.Fatalf("session budget used = %d after renewal, want 0", used)
	}
	if err := tracker.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore}); err != nil {
		t.Fatalf("next model call blocked after renewal: %v", err)
	}
}

func TestLongRunningPolicyOnlyForInteractiveRenewMode(t *testing.T) {
	renew := &Orchestrator{cfg: &config.Config{LongRunning: config.LongRunningConfig{Mode: config.LongRunningRenew}}}
	if policy, ok := renew.longRunningPolicy(context.Background()); !ok || policy.noProgressWindows != defaultNoProgressWindows {
		t.Fatalf("renew policy = %+v, %v", policy, ok)
	}
	delivery := agent.WithRunIdentity(context.Background(), agent.RunIdentity{DeliveryID: "delivery-1", RoleID: "worker", InvocationID: "run-1"})
	if _, ok := renew.longRunningPolicy(delivery); ok {
		t.Fatal("delivery workers must keep bounded accounting")
	}
	for _, mode := range []string{"", config.LongRunningBounded} {
		orch := &Orchestrator{cfg: &config.Config{LongRunning: config.LongRunningConfig{Mode: mode}}}
		if _, ok := orch.longRunningPolicy(context.Background()); ok {
			t.Fatalf("mode %q must not renew", mode)
		}
	}
}

// loopingToolProvider requests one "probe" tool call per model call until
// rounds is exhausted (negative = forever). distinct varies the arguments.
type loopingToolProvider struct {
	mu        sync.Mutex
	rounds    int
	distinct  bool
	calls     int
	deadlines []bool
}

func (p *loopingToolProvider) next(ctx context.Context) *model.ChatResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, hasDeadline := ctx.Deadline()
	p.deadlines = append(p.deadlines, hasDeadline)
	if p.rounds == 0 {
		return &model.ChatResponse{Role: model.RoleAssistant, Content: "finished", StopReason: model.StopReasonEnd}
	}
	if p.rounds > 0 {
		p.rounds--
	}
	p.calls++
	args := `{"target":"same"}`
	if p.distinct {
		args = fmt.Sprintf(`{"target":"file-%d"}`, p.calls)
	}
	return &model.ChatResponse{Role: model.RoleAssistant, StopReason: model.StopReasonToolCall,
		ToolCalls: []model.ToolCall{{ID: fmt.Sprintf("call-%d", p.calls), Name: "probe", Arguments: args}}}
}

func (p *loopingToolProvider) Chat(ctx context.Context, _ *model.ChatRequest) (*model.ChatResponse, error) {
	return p.next(ctx), nil
}

func (p *loopingToolProvider) StreamChat(ctx context.Context, _ *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	ch := make(chan *model.ChatResponse, 1)
	ch <- p.next(ctx)
	close(ch)
	return ch, nil
}

func (p *loopingToolProvider) Name() string  { return "looping" }
func (p *loopingToolProvider) Model() string { return "looping-model" }

func renewOrchestrator(t *testing.T, provider model.Provider, window config.WorkWindowConfig) (*Orchestrator, *int) {
	t.Helper()
	a := newExecutionTestAgent("coder", provider)
	probes := 0
	a.Tools.Register(&tool.Definition{Name: "probe", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectRead}, Handler: func(context.Context, map[string]any) (any, error) {
		probes++
		return "ok", nil
	}})
	cfg := &config.Config{
		Repair:      config.RepairConfig{MaxToolCalls: 5, MaxModelCalls: 2, WallTimeSec: 60, MaxTokens: 10},
		LongRunning: config.LongRunningConfig{Mode: config.LongRunningRenew, Window: window, NoProgressWindows: 2},
	}
	return &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", workspace: &workspace.Info{Root: t.TempDir()}, cfg: cfg}, &probes
}

func TestExecuteRenewModeRunsPastEveryFixedLimit(t *testing.T) {
	provider := &loopingToolProvider{rounds: 150, distinct: true}
	orch, probes := renewOrchestrator(t, provider, config.WorkWindowConfig{ToolRounds: 10, ToolCalls: 10})

	result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "long task", RequestedAgent: "coder"})
	if err != nil {
		t.Fatalf("Execute() error = %v; repair limits and the SDK cap must renew, not stop", err)
	}
	if result.Response == nil || result.Response.Content != "finished" || *probes != 150 || result.StopReason != execution.StopSuccess {
		t.Fatalf("result = %+v, probes = %d", result, *probes)
	}
	for i, hasDeadline := range provider.deadlines {
		if hasDeadline {
			t.Fatalf("model call %d ran under a wall-clock deadline in renew mode", i)
		}
	}
}

func TestExecuteRenewModePausesWithoutProgress(t *testing.T) {
	provider := &loopingToolProvider{rounds: -1}
	orch, probes := renewOrchestrator(t, provider, config.WorkWindowConfig{ToolRounds: 3})

	result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "stuck task", RequestedAgent: "coder"})
	if err != nil {
		t.Fatalf("Execute() error = %v, want a non-error pause", err)
	}
	if result.StopReason != execution.StopNoProgress || result.Response == nil || !strings.Contains(result.Response.Content, "without progress") {
		t.Fatalf("result = %+v", result)
	}
	if *probes != 9 {
		t.Fatalf("probes = %d, want 9 (one productive window, then two idle windows)", *probes)
	}
}

func TestExecuteStreamingRenewModePauseCompletesWithNoProgress(t *testing.T) {
	provider := &loopingToolProvider{rounds: -1}
	orch, _ := renewOrchestrator(t, provider, config.WorkWindowConfig{ToolRounds: 2})

	result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "stuck task", RequestedAgent: "coder", Mode: ExecutionStreaming})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for chunk := range result.Stream {
		if chunk.Err != nil {
			t.Fatalf("stream error = %v", chunk.Err)
		}
		text.WriteString(chunk.Content)
	}
	completion := <-result.Completion
	if completion.StopReason != execution.StopNoProgress || completion.Err != nil || !strings.Contains(text.String(), "without progress") {
		t.Fatalf("completion = %+v, text = %q", completion, text.String())
	}
}

func TestExecuteBoundedModeKeepsFixedIterationCap(t *testing.T) {
	provider := &loopingToolProvider{rounds: 150, distinct: true}
	orch, _ := renewOrchestrator(t, provider, config.WorkWindowConfig{})
	orch.cfg.LongRunning.Mode = config.LongRunningBounded
	orch.cfg.Repair = config.RepairConfig{}

	if _, err := orch.Execute(context.Background(), ExecutionRequest{Message: "long task", RequestedAgent: "coder"}); err == nil || !strings.Contains(err.Error(), "exceeded max tool-calling iterations") {
		t.Fatalf("Execute() error = %v, want the legacy cap in bounded mode", err)
	}
}

func TestConfiguredSubagentRenewModeHasNoWallClockAndOwnWindows(t *testing.T) {
	provider := &loopingToolProvider{rounds: 120, distinct: true}
	child, err := agent.New("child", "child").WithModel(provider).Build()
	if err != nil {
		t.Fatal(err)
	}
	child.Tools.Register(&tool.Definition{Name: "probe", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectRead}, Handler: func(context.Context, map[string]any) (any, error) {
		return "ok", nil
	}})
	runner := &configuredAgentRunner{agents: map[string]*agent.Agent{"child": child}}
	policy := &longRunningPolicy{window: workWindow{rounds: 10}, noProgressWindows: 2}
	// The parent's controller is bound to the parent; the child needs its own.
	ctx := agent.WithToolLoopController(withLongRunningPolicy(context.Background(), policy), "parent", &stopAfterRounds{})

	result, err := runner.Run(ctx, harness.SubAgentSpec{Name: "child"}, "long child task")
	if err != nil || result != "finished" {
		t.Fatalf("child result = %q, err = %v; a child past the SDK cap must renew", result, err)
	}
	for i, hasDeadline := range provider.deadlines {
		if hasDeadline {
			t.Fatalf("child model call %d had a wall-clock deadline in renew mode", i)
		}
	}
}

type stopAfterRounds struct{}

func (stopAfterRounds) AfterToolRound(context.Context, agent.ToolRound) (agent.ToolLoopAction, error) {
	return agent.ToolLoopAction{Stop: true, Message: "parent controller leaked into child"}, nil
}

func TestRenewModeDropsTheFixedRepairAttemptCap(t *testing.T) {
	limits := renewableTaskLimits(execution.TaskLimits{RepairAttempts: 1, ModelCalls: 2, ToolCalls: 3, WallTime: time.Minute, Tokens: 4, CostMicrodollars: 5})
	if limits != (execution.TaskLimits{CostMicrodollars: 5}) {
		t.Fatalf("renewable limits = %+v, want only the explicit cost ceiling", limits)
	}
}
