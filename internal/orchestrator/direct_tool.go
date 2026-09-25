package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/session"
)

// ExecuteTool invokes a registered tool on the active agent through the same
// hook and permission sequence used for model-issued tool calls.
func (o *Orchestrator) ExecuteTool(ctx context.Context, name string, args map[string]any) (any, error) {
	if o == nil {
		return nil, fmt.Errorf("orchestrator is unavailable")
	}
	active := o.ActiveAgent()
	if active == nil || active.Tools == nil {
		return nil, fmt.Errorf("no active agent")
	}

	ctx, err := o.executionTaskContext(ctx, active.ID)
	if err != nil {
		return nil, err
	}

	evt := &hooks.Event{Type: hooks.EventToolCallBefore, Name: name, Input: args}
	if err := active.Hooks.Before(ctx, evt); err != nil {
		return nil, fmt.Errorf("hook before tool %q: %w", name, err)
	}

	result, toolErr := active.Tools.Execute(ctx, name, args)
	if toolErr == nil && ctx.Err() != nil {
		toolErr = ctx.Err()
	}
	evt.Type = hooks.EventToolCallAfter
	evt.Output = result
	evt.Error = toolErr
	if err := active.Hooks.After(ctx, evt); err != nil {
		return result, errors.Join(toolErr, fmt.Errorf("hook after tool %q: %w", name, err))
	}
	return result, toolErr
}

// executionTaskContext gives direct tools, children and teams the same trusted
// task identity, runtime and effect boundary used by ordinary execution. An
// inherited worker context retains its existing identity and authority.
func (o *Orchestrator) executionTaskContext(ctx context.Context, roleID string) (context.Context, error) {
	taskID := TaskIDFromContext(ctx)
	if runtime, ok := taskRuntimeFromContext(ctx); ok {
		taskID = string(runtime.taskID)
		ctx = context.WithValue(ctx, taskIDKey{}, taskID)
	} else {
		if taskID == "" {
			taskID = session.NewSessionID()
		}
		workspaceRoot := ""
		if root, ok := builtins.WorkspaceRootFromContext(ctx); ok {
			workspaceRoot = root
		} else if o.workspace != nil {
			workspaceRoot = o.workspace.Root
		} else if o.cfg != nil {
			workspaceRoot = o.cfg.Workspace.Root
		}
		runtime, err := newTaskRuntimeWithLimits(taskID, workspaceRoot, taskLimits(o.cfg))
		if err != nil {
			return ctx, err
		}
		ctx = withTaskRuntime(ctx, runtime)
		ctx = context.WithValue(ctx, taskIDKey{}, taskID)
	}
	if storage.SessionFromContext(ctx) == "" {
		if sessionID := o.sessionID(o.active); sessionID != "" {
			ctx = storage.WithSession(ctx, sessionID)
		}
	}
	if _, ok := agent.RunIdentityFromContext(ctx); !ok {
		identity := agent.RunIdentity{TaskID: taskID, RoleID: roleID, SessionID: storage.SessionFromContext(ctx), InvocationID: session.NewSessionID()}
		if authorized, ok := authorization.FromContext(ctx); ok {
			identity.TenantID, identity.RepositoryID = authorized.TenantID, authorized.RepositoryID
		}
		ctx = agent.WithRunIdentity(ctx, identity)
	}
	ctx = executionEffectContext(ctx, o.PlanMode())
	return ctx, nil
}

func executionEffectContext(ctx context.Context, planOnly bool) context.Context {
	if _, constrained := tool.EffectGrantFromContext(ctx); constrained {
		return ctx
	}
	if planOnly {
		return tool.WithEffectGrant(ctx, tool.EffectRead, tool.EffectScratchWrite)
	}
	return tool.WithEffectGrant(ctx,
		tool.EffectRead,
		tool.EffectScratchWrite,
		tool.EffectDeliveryWrite,
		tool.EffectProcessExecution,
		tool.EffectNetwork,
		tool.EffectExternalMutation,
	)
}
