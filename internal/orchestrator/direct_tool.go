package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/storage"

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

	taskID := TaskIDFromContext(ctx)
	if runtime, ok := taskRuntimeFromContext(ctx); ok {
		if taskID == "" {
			taskID = string(runtime.taskID)
			ctx = context.WithValue(ctx, taskIDKey{}, taskID)
		}
	} else {
		if taskID == "" {
			taskID = session.NewSessionID()
			ctx = context.WithValue(ctx, taskIDKey{}, taskID)
		}
		workspaceRoot := ""
		if o.workspace != nil {
			workspaceRoot = o.workspace.Root
		} else if o.cfg != nil {
			workspaceRoot = o.cfg.Workspace.Root
		}
		runtime, err := newTaskRuntimeWithLimits(taskID, workspaceRoot, taskLimits(o.cfg))
		if err != nil {
			return nil, err
		}
		ctx = withTaskRuntime(ctx, runtime)
	}
	if storage.SessionFromContext(ctx) == "" {
		if sessionID := o.CurrentSessionID(); sessionID != "" {
			ctx = storage.WithSession(ctx, sessionID)
		}
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
