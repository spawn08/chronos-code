package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spawn08/chronos/engine/guardrails"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/harness"
)

type resourceRunnerFunc func(context.Context, harness.SubAgentSpec, string) (string, error)

func (f resourceRunnerFunc) Run(ctx context.Context, spec harness.SubAgentSpec, task string) (string, error) {
	return f(ctx, spec, task)
}

type resourceProvider struct {
	chat func(context.Context, *model.ChatRequest) (*model.ChatResponse, error)
}

func (p resourceProvider) Chat(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	return p.chat(ctx, req)
}

func (resourceProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, errors.New("unexpected streaming call")
}

func (resourceProvider) Name() string  { return "resource-test" }
func (resourceProvider) Model() string { return "resource-test" }

func resourceReply(content string) *model.ChatResponse {
	return &model.ChatResponse{Role: model.RoleAssistant, Content: content, StopReason: model.StopReasonEnd}
}

func resourceAgent(t *testing.T, name string, provider model.Provider) *agent.Agent {
	t.Helper()
	a, err := agent.New(name, name).WithModel(provider).Build()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func resourceContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func resourceReceive[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatalf("waiting for delegated run: %v", ctx.Err())
		var zero T
		return zero
	}
}

func TestConfiguredSubagentInheritsIsolatedWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	var observed string
	child := resourceAgent(t, "child", resourceProvider{chat: func(ctx context.Context, _ *model.ChatRequest) (*model.ChatResponse, error) {
		observed, _ = builtins.WorkspaceRootFromContext(ctx)
		return resourceReply("done"), nil
	}})
	runner := &configuredAgentRunner{agents: map[string]*agent.Agent{"child": child}}
	if _, err := runner.Run(builtins.WithWorkspaceRoot(context.Background(), root), harness.SubAgentSpec{Name: "child"}, "inspect"); err != nil {
		t.Fatal(err)
	}
	if observed != root {
		t.Fatalf("subagent workspace root = %q, want %q", observed, root)
	}
}

func TestConfiguredSubagentDerivesInvocationIdentity(t *testing.T) {
	var observed agent.RunIdentity
	child := resourceAgent(t, "child", resourceProvider{chat: func(ctx context.Context, _ *model.ChatRequest) (*model.ChatResponse, error) {
		observed, _ = agent.RunIdentityFromContext(ctx)
		return resourceReply("done"), nil
	}})
	runner := &configuredAgentRunner{agents: map[string]*agent.Agent{"child": child}}
	parent := agent.RunIdentity{TaskID: "task-1", DeliveryID: "delivery-1", RoleID: "parent", InvocationID: "parent-run"}
	if _, err := runner.Run(agent.WithRunIdentity(context.Background(), parent), harness.SubAgentSpec{Name: "child"}, "inspect"); err != nil {
		t.Fatal(err)
	}
	if observed.TaskID != parent.TaskID || observed.DeliveryID != parent.DeliveryID || observed.RoleID != "child" || observed.ParentInvocationID != parent.InvocationID || observed.InvocationID == "" || observed.InvocationID == parent.InvocationID {
		t.Fatalf("child identity = %+v, parent = %+v", observed, parent)
	}
}

func TestSubagentResourcesAggregateAcrossParents(t *testing.T) {
	ctx := resourceContext(t)
	resources := &subagentResources{gate: make(chan struct{}, 3)}
	var active, peak atomic.Int32
	started := make(chan struct{}, 24)
	release := make(chan struct{})
	fallback := resourceRunnerFunc(func(ctx context.Context, _ harness.SubAgentSpec, _ string) (string, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	runners := []*configuredAgentRunner{
		{resources: resources, fallback: fallback},
		{resources: resources, fallback: fallback},
	}
	done := make(chan error, 24)
	for i := 0; i < 24; i++ {
		go func(i int) {
			_, err := runners[i%2].Run(ctx, harness.SubAgentSpec{Name: "dynamic"}, "task")
			done <- err
		}(i)
	}
	for i := 0; i < 3; i++ {
		resourceReceive(t, ctx, started)
	}
	close(release)
	for i := 0; i < 24; i++ {
		if err := resourceReceive(t, ctx, done); err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 3 || active.Load() != 0 || len(resources.gate) != 0 {
		t.Fatalf("peak=%d active=%d permits=%d", peak.Load(), active.Load(), len(resources.gate))
	}
}

func TestSubagentNestedFanoutIsBoundedAndFailsFast(t *testing.T) {
	for _, capacity := range []int{1, 3} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			ctx := resourceContext(t)
			resources := &subagentResources{gate: make(chan struct{}, capacity)}
			const children = 12
			outcomes := make(chan error, children)
			release := make(chan struct{})
			child := &configuredAgentRunner{resources: resources, fallback: resourceRunnerFunc(func(ctx context.Context, _ harness.SubAgentSpec, _ string) (string, error) {
				outcomes <- nil // admitted; hold the permit until every sibling tried
				select {
				case <-release:
					return "child", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			})}
			parent := &configuredAgentRunner{resources: resources, fallback: resourceRunnerFunc(func(ctx context.Context, _ harness.SubAgentSpec, _ string) (string, error) {
				var wg sync.WaitGroup
				for i := 0; i < children; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if _, err := child.Run(ctx, harness.SubAgentSpec{Name: "child"}, "task"); err != nil {
							outcomes <- err
						}
					}()
				}
				wg.Wait()
				return "parent", nil
			})}
			done := make(chan error, 1)
			go func() {
				_, err := parent.Run(ctx, harness.SubAgentSpec{Name: "parent"}, "task")
				done <- err
			}()
			admitted := 0
			for i := 0; i < children; i++ {
				err := resourceReceive(t, ctx, outcomes)
				if err == nil {
					admitted++
				} else if !errors.Is(err, errSubagentBusy) {
					t.Fatalf("nested error = %v", err)
				}
			}
			if admitted != capacity-1 {
				t.Fatalf("admitted %d children with capacity %d (parent holds one)", admitted, capacity)
			}
			close(release)
			if err := resourceReceive(t, ctx, done); err != nil {
				t.Fatal(err)
			}
			if len(resources.gate) != 0 {
				t.Fatal("nested requests leaked permits")
			}
		})
	}
}

func TestSubagentCapacityCancellationAndDeadline(t *testing.T) {
	ctx := resourceContext(t)
	resources := &subagentResources{gate: make(chan struct{}, 1)}
	entered := make(chan context.Context, 1)
	var calls atomic.Int32
	runner := &configuredAgentRunner{resources: resources, fallback: resourceRunnerFunc(func(ctx context.Context, _ harness.SubAgentSpec, task string) (string, error) {
		calls.Add(1)
		if task == "hold" {
			entered <- ctx
			<-ctx.Done()
			return "late success", nil // cancellation must win over a late result
		}
		return task, nil
	})}
	activeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(activeCtx, harness.SubAgentSpec{Name: "dynamic"}, "hold")
		done <- err
	}()
	childCtx := resourceReceive(t, ctx, entered)
	if deadline, ok := childCtx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
		t.Fatalf("parent deadline not inherited: %v, %v", deadline, ok)
	}
	waitCtx, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	if _, err := runner.Run(waitCtx, harness.SubAgentSpec{Name: "queued"}, "task"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued deadline error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("queued request invoked fallback")
	}
	cancel()
	if err := resourceReceive(t, ctx, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("active cancellation error = %v", err)
	}
	if _, err := runner.Run(activeCtx, harness.SubAgentSpec{Name: "canceled"}, "task"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled request error = %v", err)
	}
	if result, err := runner.Run(ctx, harness.SubAgentSpec{Name: "fresh"}, "fresh"); err != nil || result != "fresh" {
		t.Fatalf("permit not reusable: %q, %v", result, err)
	}
	if calls.Load() != 2 || len(resources.gate) != 0 {
		t.Fatalf("calls=%d permits=%d", calls.Load(), len(resources.gate))
	}
}

func TestSetupSubagentsRunsSameRoleWithIsolatedInvocationState(t *testing.T) {
	ctx := resourceContext(t)
	type observation struct {
		task      string
		identity  agent.RunIdentity
		workspace string
		messages  int
	}
	entered := make(chan observation, 2)
	var calls atomic.Int32
	provider := resourceProvider{chat: func(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
		calls.Add(1)
		task := req.Messages[len(req.Messages)-1].Content
		identity, _ := agent.RunIdentityFromContext(ctx)
		workspace, _ := builtins.WorkspaceRootFromContext(ctx)
		entered <- observation{task: task, identity: identity, workspace: workspace, messages: len(req.Messages)}
		<-ctx.Done()
		if !errors.Is(ctx.Err(), context.Canceled) {
			return nil, ctx.Err()
		}
		return resourceReply(task), nil
	}}
	a, b, worker := resourceAgent(t, "a", provider), resourceAgent(t, "b", provider), resourceAgent(t, "worker", provider)
	a.SubAgents = []*agent.Agent{worker}
	b.SubAgents = []*agent.Agent{worker}
	if err := setupSubAgents(map[string]*agent.Agent{"a": a, "b": b, "worker": worker}); err != nil {
		t.Fatal(err)
	}
	spawn := func(ctx context.Context, parent *agent.Agent, task string) error {
		_, err := parent.Tools.Execute(ctx, harness.SpawnToolName, map[string]any{"agent": "worker", "task": task})
		return err
	}
	activeCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 2)
	parentA := agent.WithRunIdentity(builtins.WithWorkspaceRoot(activeCtx, "/workspace/a"), agent.RunIdentity{TaskID: "task", RoleID: "a", InvocationID: "parent-a"})
	parentB := agent.WithRunIdentity(builtins.WithWorkspaceRoot(activeCtx, "/workspace/b"), agent.RunIdentity{TaskID: "task", RoleID: "b", InvocationID: "parent-b"})
	go func() { done <- spawn(parentA, a, "first") }()
	go func() { done <- spawn(parentB, b, "second") }()
	first := resourceReceive(t, ctx, entered)
	second := resourceReceive(t, ctx, entered)
	if first.task == second.task || first.identity.InvocationID == second.identity.InvocationID || first.workspace == second.workspace {
		t.Fatalf("same-role invocations leaked state: first=%+v second=%+v", first, second)
	}
	for _, observed := range []observation{first, second} {
		if observed.messages != 1 || observed.identity.RoleID != "worker" || observed.identity.ParentInvocationID == "" {
			t.Fatalf("invalid invocation observation: %+v", observed)
		}
	}
	cancel()
	for range 2 {
		if err := resourceReceive(t, ctx, done); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2", calls.Load())
	}
}

func TestSetupSubAgentsHonorsDeclaredChildren(t *testing.T) {
	provider := resourceProvider{chat: func(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
		return resourceReply("done"), nil
	}}
	parent := resourceAgent(t, "parent", provider)
	allowed := resourceAgent(t, "allowed", provider)
	forbidden := resourceAgent(t, "forbidden", provider)
	parent.SubAgents = []*agent.Agent{allowed}
	if err := setupSubAgents(map[string]*agent.Agent{"parent": parent, "allowed": allowed, "forbidden": forbidden}); err != nil {
		t.Fatal(err)
	}
	definition, ok := parent.Tools.Get(harness.SpawnToolName)
	if !ok {
		t.Fatal("spawn_subagent tool is missing")
	}
	if !strings.Contains(definition.Description, "allowed") || strings.Contains(definition.Description, "forbidden") {
		t.Fatalf("spawn_subagent description = %q", definition.Description)
	}
	if _, err := parent.Tools.Execute(context.Background(), harness.SpawnToolName, map[string]any{"agent": "forbidden", "task": "inspect"}); err == nil {
		t.Fatal("undeclared configured peer was executable")
	}
}

func TestSubagentBoundsAndErrorLeaseRelease(t *testing.T) {
	ctx := resourceContext(t)
	var calls int
	wantErr := errors.New("execution failed")
	runner := &configuredAgentRunner{fallback: resourceRunnerFunc(func(ctx context.Context, _ harness.SubAgentSpec, task string) (string, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("fallback has no deadline")
		}
		switch task {
		case "large":
			return strings.Repeat("界", subagentResultLimit/3+1), nil
		case "error":
			return "", wantErr
		default:
			return task, nil
		}
	})}
	for _, spec := range []harness.SubAgentSpec{
		{SystemPrompt: strings.Repeat("x", subagentTaskLimit)},
		{Instructions: []string{strings.Repeat("x", subagentTaskLimit)}},
		{ToolNames: []string{strings.Repeat("x", subagentTaskLimit)}},
	} {
		_, err := runner.Run(ctx, spec, "task")
		var sizeErr *subagentSizeError
		if !errors.As(err, &sizeErr) || sizeErr.Limit != subagentTaskLimit || sizeErr.Preview != "" {
			t.Fatalf("oversized spec error = %v", err)
		}
	}
	if _, err := runner.Run(ctx, harness.SubAgentSpec{}, strings.Repeat("x", subagentTaskLimit+1)); err == nil || calls != 0 {
		t.Fatalf("oversized task: err=%v calls=%d", err, calls)
	}
	if result, err := runner.Run(ctx, harness.SubAgentSpec{}, "large"); result != "" || err == nil {
		t.Fatalf("oversized result: len=%d err=%v", len(result), err)
	} else {
		var sizeErr *subagentSizeError
		if !errors.As(err, &sizeErr) || sizeErr.Resource != "result" || len(sizeErr.Preview) > subagentPreviewLimit || !utf8.ValidString(sizeErr.Preview) {
			t.Fatalf("invalid result size error: %v", err)
		}
	}
	if _, err := runner.Run(ctx, harness.SubAgentSpec{}, "error"); !errors.Is(err, wantErr) {
		t.Fatalf("lost execution error: %v", err)
	}
	cycleCtx := context.WithValue(ctx, subagentPathKey{}, []string{"root", "child"})
	if _, err := runner.Run(cycleCtx, harness.SubAgentSpec{Name: "root"}, "cycle"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
	boundary := strings.Repeat("x", subagentTaskLimit)
	if result, err := runner.Run(ctx, harness.SubAgentSpec{}, boundary); err != nil || result != boundary {
		t.Fatalf("boundary task was changed: len=%d err=%v", len(result), err)
	}
	if len(runner.resources.gate) != 0 {
		t.Fatal("error path leaked permits")
	}
}

type resourceBudgetHook struct{}

func (resourceBudgetHook) Before(ctx context.Context, evt *hooks.Event) error {
	if evt.Type == hooks.EventModelCallBefore {
		return claimTurnModelCall(ctx)
	}
	return nil
}

func (resourceBudgetHook) After(context.Context, *hooks.Event) error { return nil }

func TestConfiguredSubagentRetainsHooksToolsGuardrailsAndBudget(t *testing.T) {
	ctx := resourceContext(t)
	var providerCalls, toolCalls int
	provider := resourceProvider{chat: func(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
		providerCalls++
		last := req.Messages[len(req.Messages)-1]
		if last.Role == model.RoleUser {
			if last.Content == "bad output" {
				return resourceReply("forbidden output"), nil
			}
			var system []string
			for _, message := range req.Messages {
				if message.Role == model.RoleSystem {
					system = append(system, message.Content)
				}
			}
			if got := strings.Join(system, "|"); got != "configured role|configured instruction|configured pin" {
				return nil, fmt.Errorf("lost configured context: %q", got)
			}
			return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{
				{ID: "allowed", Name: "configured_tool", Arguments: "{}"},
				{ID: "denied", Name: "denied_tool", Arguments: "{}"},
			}}, nil
		}
		if last.Name != "denied_tool" || !strings.Contains(strings.ToLower(last.Content), "denied") {
			return nil, fmt.Errorf("lost configured security policy: %+v", last)
		}
		return resourceReply("done"), nil
	}}
	worker := resourceAgent(t, "worker", provider)
	worker.SystemPrompt = "configured role"
	worker.SessionState["instruction"] = "configured instruction"
	worker.InstructionsFn = func(_ context.Context, state map[string]any) []string {
		return []string{state["instruction"].(string)}
	}
	worker.ContextPinsFn = func(context.Context) []model.Message {
		return []model.Message{{Role: model.RoleSystem, Content: "configured pin"}}
	}
	log := &hooks.LoggingHook{}
	worker.Hooks = hooks.Chain{resourceBudgetHook{}, log}
	worker.Guardrails.AddRule(guardrails.Rule{Name: "input", Position: guardrails.Input, Guardrail: &guardrails.BlocklistGuardrail{Blocklist: []string{"blocked task"}}})
	worker.Guardrails.AddRule(guardrails.Rule{Name: "output", Position: guardrails.Output, Guardrail: &guardrails.BlocklistGuardrail{Blocklist: []string{"forbidden output"}}})
	worker.Tools.Register(&tool.Definition{Name: "configured_tool", Permission: tool.PermAllow, Handler: func(context.Context, map[string]any) (any, error) {
		toolCalls++
		return "configured result", nil
	}})
	worker.Tools.Register(&tool.Definition{Name: "denied_tool", Permission: tool.PermDeny, Handler: func(context.Context, map[string]any) (any, error) {
		t.Error("denied tool handler executed")
		return nil, nil
	}})
	runner := &configuredAgentRunner{agents: map[string]*agent.Agent{"worker": worker}}
	spec := harness.SubAgentSpec{Name: "worker"}
	turnCtx := withSubagentTurnState(ctx, 2, "root")
	if _, err := runner.Run(turnCtx, spec, "blocked task"); err == nil || !strings.Contains(err.Error(), "input guardrail") {
		t.Fatalf("input guardrail error = %v", err)
	}
	if result, err := runner.Run(turnCtx, spec, "task"); err != nil || result != "done" {
		t.Fatalf("configured execution = %q, %v", result, err)
	}
	if providerCalls != 2 || toolCalls != 1 || len(log.Events) == 0 {
		t.Fatalf("provider calls=%d tool calls=%d hook events=%d", providerCalls, toolCalls, len(log.Events))
	}
	if _, err := runner.Run(turnCtx, spec, "next task"); err == nil || !strings.Contains(err.Error(), "model call limit") {
		t.Fatalf("shared turn budget error = %v", err)
	}
	if providerCalls != 2 {
		t.Fatal("exhausted budget invoked provider")
	}
	if _, err := runner.Run(ctx, spec, "bad output"); err == nil || !strings.Contains(err.Error(), "output guardrail") {
		t.Fatalf("output guardrail error = %v", err)
	}
	if result, err := runner.Run(ctx, spec, "fresh task"); err != nil || result != "done" {
		t.Fatalf("leases not reusable after guardrail/budget errors: %q, %v", result, err)
	}
}

func TestSubagentSDKDepthLimitRetained(t *testing.T) {
	ctx := resourceContext(t)
	parent := resourceAgent(t, "parent", resourceProvider{})
	svc, err := harness.NewSubAgentService(parent)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= harness.DefaultMaxSubAgentDepth; i++ {
		if err := svc.Register(harness.SubAgentSpec{Name: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	runner := &configuredAgentRunner{fallback: resourceRunnerFunc(func(ctx context.Context, _ harness.SubAgentSpec, _ string) (string, error) {
		calls++
		_, err := parent.Tools.Execute(ctx, harness.SpawnToolName, map[string]any{"agent": fmt.Sprint(calls), "task": "nested"})
		return "", err
	})}
	harness.Attach(svc, runner)
	_, err = parent.Tools.Execute(ctx, harness.SpawnToolName, map[string]any{"agent": "0", "task": "root"})
	if err == nil || !strings.Contains(err.Error(), "maximum subagent depth") || calls != harness.DefaultMaxSubAgentDepth {
		t.Fatalf("depth error=%v calls=%d", err, calls)
	}
	if len(runner.resources.gate) != 0 {
		t.Fatal("depth error leaked permits")
	}
}

func TestSetupSubagentsPreservesDynamicFallback(t *testing.T) {
	var calls int
	provider := resourceProvider{chat: func(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > boundedSubagentTimeout || time.Until(deadline) < boundedSubagentTimeout-time.Minute {
			return nil, fmt.Errorf("missing default delegation deadline: %v", deadline)
		}
		if req.Messages[0].Content != "dynamic role" || req.Messages[len(req.Messages)-1].Content != "dynamic task" {
			return nil, fmt.Errorf("dynamic spec/task lost: %+v", req.Messages)
		}
		return resourceReply("dynamic result"), nil
	}}
	parent := resourceAgent(t, "parent", provider)
	if err := setupSubAgents(map[string]*agent.Agent{"parent": parent}); err != nil {
		t.Fatal(err)
	}
	result, err := parent.Tools.Execute(context.Background(), harness.SpawnToolName, map[string]any{"system_prompt": "dynamic role", "task": "dynamic task"})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["result"] != "dynamic result" || calls != 1 {
		t.Fatalf("dynamic fallback result=%v calls=%d", result, calls)
	}
}

func TestDelegationAttenuatesEffectGrant(t *testing.T) {
	ctx := tool.WithEffectGrant(context.Background(), tool.EffectRead, tool.EffectDeliveryWrite)
	definitions := []*tool.Definition{
		{Name: "read", Effects: []tool.Effect{tool.EffectRead}},
		{Name: "network", Effects: []tool.Effect{tool.EffectNetwork}},
	}
	grant, ok := tool.EffectGrantFromContext(attenuateEffectGrant(ctx, definitions))
	if !ok {
		t.Fatal("attenuated grant missing")
	}
	if len(grant) != 1 {
		t.Fatalf("attenuated grant = %v", grant)
	}
	if _, ok := grant[tool.EffectRead]; !ok {
		t.Fatal("delegated read effect removed")
	}
	if _, ok := grant[tool.EffectDeliveryWrite]; ok {
		t.Fatal("child gained unused parent delivery-write authority")
	}
	if _, ok := grant[tool.EffectNetwork]; ok {
		t.Fatal("child gained network authority absent from parent")
	}
}
