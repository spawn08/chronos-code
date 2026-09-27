package orchestrator

import (
	"context"
	"strings"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/stream"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

const taskPlanHeader = "Current task plan (update_plan; replace it when you start a different task):\n"

// taskPlanStore scopes the update_plan list. A delegated child inherits the
// parent's session context, so it gets a list keyed by its own invocation;
// otherwise a child's update_plan call would overwrite the parent's plan.
type taskPlanStore struct {
	plans *builtins.InMemoryPlanStore
}

func newTaskPlanStore() *taskPlanStore {
	return &taskPlanStore{plans: builtins.NewInMemoryPlanStore()}
}

func (s *taskPlanStore) scope(ctx context.Context) context.Context {
	if identity, ok := agent.RunIdentityFromContext(ctx); ok && identity.ParentInvocationID != "" && identity.InvocationID != "" {
		return storage.WithSession(ctx, "subagent:"+identity.InvocationID)
	}
	return ctx
}

func (s *taskPlanStore) Load(ctx context.Context) (*builtins.Plan, error) {
	return s.plans.Load(s.scope(ctx))
}

func (s *taskPlanStore) Save(ctx context.Context, plan *builtins.Plan) error {
	return s.plans.Save(s.scope(ctx), plan)
}

// openItems returns the unfinished tasks of the primary plan for sessionID.
func (s *taskPlanStore) openItems(ctx context.Context, sessionID string) []builtins.PlanTask {
	if s == nil || sessionID == "" {
		return nil
	}
	plan, err := s.plans.Load(storage.WithSession(ctx, sessionID))
	if err != nil || plan == nil {
		return nil
	}
	var open []builtins.PlanTask
	for _, task := range plan.Tasks {
		if task.Status != builtins.TaskCompleted {
			open = append(open, task)
		}
	}
	return open
}

// installTaskPlan gives every agent that can write files the update_plan tool.
func installTaskPlan(agents map[string]*agent.Agent, store *taskPlanStore, broker *stream.Broker) {
	for _, a := range agents {
		if a == nil || a.Tools == nil {
			continue
		}
		if _, writes := a.Tools.Get("file_write"); !writes {
			continue
		}
		a.Tools.Register(builtins.NewPlanTool(store, broker))
		a.Hooks = append(a.Hooks, taskPlanHook{store: store})
	}
}

// taskPlanHook shows the current plan on every model call, so it stays
// visible after compaction and reflects updates made earlier in the turn.
type taskPlanHook struct {
	store *taskPlanStore
}

func (h taskPlanHook) Before(ctx context.Context, evt *hooks.Event) error {
	if evt == nil || evt.Type != hooks.EventModelCallBefore || h.store == nil {
		return nil
	}
	req, ok := evt.Input.(*model.ChatRequest)
	if !ok || req == nil || storage.SessionFromContext(ctx) == "" {
		return nil
	}
	plan, err := h.store.Load(ctx)
	if err != nil || plan == nil {
		return nil
	}
	// The SDK carries a round's request messages into the next round, so drop
	// the previous pin instead of stacking one per call.
	messages := make([]model.Message, 0, len(req.Messages)+1)
	for _, message := range req.Messages {
		if message.Role == model.RoleSystem && strings.HasPrefix(message.Content, taskPlanHeader) {
			continue
		}
		messages = append(messages, message)
	}
	if len(plan.Tasks) > 0 {
		prefix := 0
		for prefix < len(messages) && messages[prefix].Role == model.RoleSystem {
			prefix++
		}
		pin := model.Message{Role: model.RoleSystem, Content: taskPlanHeader + plan.Summary()}
		messages = append(messages[:prefix], append([]model.Message{pin}, messages[prefix:]...)...)
	}
	req.Messages = messages
	return nil
}

func (taskPlanHook) After(context.Context, *hooks.Event) error { return nil }
