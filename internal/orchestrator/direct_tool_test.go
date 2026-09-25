package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
)

type directToolHook struct {
	before    int
	after     int
	context   context.Context
	afterErr  error
	toolError error
}

func (h *directToolHook) Before(ctx context.Context, _ *hooks.Event) error {
	h.before++
	h.context = ctx
	return nil
}

func (h *directToolHook) After(_ context.Context, evt *hooks.Event) error {
	h.after++
	h.toolError = evt.Error
	return h.afterErr
}

func TestExecuteToolUsesRegistryPolicyForDeniedShell(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	orch.active = "coder"
	orch.sessions = map[string]string{"coder": "session-1"}
	executions := 0
	registerApprovalTool(orch.ActiveAgent().Tools, "shell", &executions)

	_, err := orch.ExecuteTool(context.Background(), "shell", map[string]any{"command": "rm dangerous"})
	if err == nil || !strings.Contains(err.Error(), "approval denied") {
		t.Fatalf("ExecuteTool() error = %v, want policy denial", err)
	}
	if executions != 0 {
		t.Fatalf("shell executions = %d, want 0", executions)
	}
}

func TestExecuteToolPlanModeBlocksShellThroughSessionHook(t *testing.T) {
	executions := 0
	a := &agent.Agent{ID: "coder", Tools: tool.NewRegistry()}
	registerPermissionTool(a.Tools, "shell", tool.PermAllow, &executions)
	orch := &Orchestrator{
		agents:   map[string]*agent.Agent{"coder": a},
		active:   "coder",
		sessions: map[string]string{"coder": "session-1"},
	}
	a.Hooks = append(a.Hooks, sessionUXHook{orchestrator: orch})
	orch.SetPlanMode(true)

	_, err := orch.ExecuteTool(context.Background(), "shell", map[string]any{"command": "pwd"})
	if err == nil || !strings.Contains(err.Error(), "plan mode") {
		t.Fatalf("ExecuteTool() error = %v, want plan-mode denial", err)
	}
	if executions != 0 {
		t.Fatalf("shell executions = %d, want 0", executions)
	}
}

func TestExecuteToolPropagatesCancellationAndTimeoutWithRuntimeEvidence(t *testing.T) {
	tests := []struct {
		name     string
		context  func() (context.Context, context.CancelFunc)
		wantErr  error
		terminal execution.TerminalState
	}{
		{
			name: "cancelled",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			wantErr:  context.Canceled,
			terminal: execution.TerminalCancelled,
		},
		{
			name: "timed out",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), time.Nanosecond)
			},
			wantErr:  context.DeadlineExceeded,
			terminal: execution.TerminalTimedOut,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &agent.Agent{ID: "coder", Tools: tool.NewRegistry()}
			a.Tools.Register(&tool.Definition{
				Name:       "shell",
				Permission: tool.PermAllow,
				Effects:    []tool.Effect{tool.EffectProcessExecution},
				Handler: func(ctx context.Context, _ map[string]any) (any, error) {
					<-ctx.Done()
					return map[string]any{"exit_code": -1}, nil
				},
			})
			wrapVerificationEvidence(a)
			hook := &directToolHook{}
			a.Hooks = append(a.Hooks, hook)
			orch := &Orchestrator{
				agents:   map[string]*agent.Agent{"coder": a},
				active:   "coder",
				sessions: map[string]string{"coder": "session-1"},
				cfg:      &config.Config{Workspace: config.WorkspaceConfig{Root: t.TempDir()}},
			}
			ctx, cancel := tt.context()
			defer cancel()

			_, err := orch.ExecuteTool(ctx, "shell", map[string]any{"command": "go test ./..."})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ExecuteTool() error = %v, want %v", err, tt.wantErr)
			}
			if hook.before != 1 || hook.after != 1 || !errors.Is(hook.toolError, tt.wantErr) {
				t.Fatalf("hook calls = before %d, after %d, tool error %v", hook.before, hook.after, hook.toolError)
			}
			if TaskIDFromContext(hook.context) == "" || storage.SessionFromContext(hook.context) != "session-1" {
				t.Fatalf("direct tool context missing task/session identity")
			}
			runtime, ok := taskRuntimeFromContext(hook.context)
			if !ok {
				t.Fatal("direct tool context missing task runtime")
			}
			state, snapshotErr := runtime.snapshot()
			if snapshotErr != nil {
				t.Fatalf("runtime snapshot: %v", snapshotErr)
			}
			if len(state.Events) != 1 || state.Events[0].TerminalState != tt.terminal {
				t.Fatalf("runtime events = %#v, want %s shell evidence", state.Events, tt.terminal)
			}
		})
	}
}

func TestExecuteToolReturnsAfterHookFailure(t *testing.T) {
	a := &agent.Agent{ID: "coder", Tools: tool.NewRegistry()}
	a.Tools.Register(&tool.Definition{
		Name:       "shell",
		Permission: tool.PermAllow,
		Effects:    []tool.Effect{tool.EffectProcessExecution},
		Handler: func(context.Context, map[string]any) (any, error) {
			return map[string]any{"stdout": "complete\n", "exit_code": 0}, nil
		},
	})
	afterErr := errors.New("after failed")
	hook := &directToolHook{afterErr: afterErr}
	a.Hooks = append(a.Hooks, hook)
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder"}

	result, err := orch.ExecuteTool(context.Background(), "shell", map[string]any{"command": "pwd"})
	if !errors.Is(err, afterErr) {
		t.Fatalf("ExecuteTool() error = %v, want after-hook error", err)
	}
	if result == nil || hook.before != 1 || hook.after != 1 {
		t.Fatalf("result = %#v, hooks = before %d after %d", result, hook.before, hook.after)
	}
}

func TestDirectSubagentUsesToolHooksAndInheritsWorkerContext(t *testing.T) {
	parent := resourceAgent(t, "parent", resourceProvider{chat: func(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
		return resourceReply("parent"), nil
	}})
	var childCtx context.Context
	child := resourceAgent(t, "child", resourceProvider{chat: func(ctx context.Context, _ *model.ChatRequest) (*model.ChatResponse, error) {
		childCtx = ctx
		return resourceReply("done"), nil
	}})
	child.Tools.Register(&tool.Definition{Name: "inspect", Effects: []tool.Effect{tool.EffectRead}, Handler: func(context.Context, map[string]any) (any, error) {
		return "inspected", nil
	}})
	parent.SubAgents = []*agent.Agent{child}
	if err := setupSubAgents(map[string]*agent.Agent{"parent": parent, "child": child}); err != nil {
		t.Fatal(err)
	}
	hook := &directToolHook{}
	parent.Hooks = append(parent.Hooks, hook)
	orch := &Orchestrator{agents: map[string]*agent.Agent{"parent": parent, "child": child}, active: "parent", sessions: map[string]string{"parent": "session"}}
	runtime, err := newTaskRuntime("delivery", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker := agent.RunIdentity{TenantID: "tenant", RepositoryID: "repo", DeliveryID: "delivery", TaskID: "delivery", RoleID: "parent", InvocationID: "worker-run"}
	ctx := withTaskRuntime(context.Background(), runtime)
	ctx = agent.WithRunIdentity(ctx, worker)
	ctx = authorization.WithRequest(ctx, authorization.Request{PrincipalID: "worker", TenantID: "tenant", RepositoryID: "repo", Action: "delivery.execute"})
	ctx = tool.WithEffectGrant(ctx, tool.EffectRead)
	result, err := orch.RunSubagent(ctx, map[string]any{"agent": "child", "task": "inspect"})
	if err != nil || result != "done" {
		t.Fatalf("RunSubagent() = %q, %v", result, err)
	}
	if hook.before != 1 || hook.after != 1 || hook.toolError != nil {
		t.Fatalf("spawn hooks: before=%d after=%d error=%v", hook.before, hook.after, hook.toolError)
	}
	identity, ok := agent.RunIdentityFromContext(childCtx)
	if !ok || identity.TaskID != "delivery" || identity.DeliveryID != "delivery" || identity.ParentInvocationID != worker.InvocationID || identity.RoleID != "child" {
		t.Fatalf("child identity = %+v, ok=%v", identity, ok)
	}
	if inherited, ok := taskRuntimeFromContext(childCtx); !ok || inherited != runtime {
		t.Fatal("child did not inherit worker task runtime")
	}
	grant, ok := tool.EffectGrantFromContext(childCtx)
	if !ok || len(grant) != 1 {
		t.Fatalf("child effect grant = %v, ok=%v", grant, ok)
	}
	if _, ok := grant[tool.EffectRead]; !ok {
		t.Fatalf("child lost read grant: %v", grant)
	}
	if hook.context == nil || TaskIDFromContext(hook.context) != "delivery" {
		t.Fatal("spawn hook did not receive worker task identity")
	}
}
