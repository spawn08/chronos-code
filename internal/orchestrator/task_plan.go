package orchestrator

import (
	"context"
	"strings"
	"sync"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/stream"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

const priorTaskPlanHeader = "Previous task plan (reference only; adopt relevant work with update_plan for the current task):\n"

const taskPlanHeader = "Current task plan (update_plan; replace it when you start a different task):\n"

// taskPlanStore scopes the update_plan list. A delegated child inherits the
// parent's session context, so it gets a list keyed by its own invocation;
// otherwise a child's update_plan call would overwrite the parent's plan.
//
// Primary-session plans are also written through to an optional durable store
// so a resumed session gets its checklist back after a restart. The in-memory
// copy stays authoritative within a process: a failed durable write never
// fails update_plan, and a session already touched here never re-reads a
// possibly stale durable copy.
type taskPlanStore struct {
	plans   *builtins.InMemoryPlanStore
	durable builtins.PlanStore

	mu     sync.Mutex
	loaded map[string]bool
	owners map[string]string
}

func newTaskPlanStore() *taskPlanStore {
	return newDurableTaskPlanStore(nil)
}

func newDurableTaskPlanStore(durable builtins.PlanStore) *taskPlanStore {
	return &taskPlanStore{plans: builtins.NewInMemoryPlanStore(), durable: durable, loaded: make(map[string]bool), owners: make(map[string]string)}
}

// scope returns the plan context and whether it is a primary session, the
// only scope that exists in durable session storage.
func (s *taskPlanStore) scope(ctx context.Context) (context.Context, bool) {
	if identity, ok := agent.RunIdentityFromContext(ctx); ok && identity.ParentInvocationID != "" && identity.InvocationID != "" {
		return storage.WithSession(ctx, "subagent:"+identity.InvocationID), false
	}
	return ctx, true
}

func (s *taskPlanStore) Load(ctx context.Context) (*builtins.Plan, error) {
	scoped, primary := s.scope(ctx)
	return s.load(scoped, primary)
}

func (s *taskPlanStore) Save(ctx context.Context, plan *builtins.Plan) error {
	scoped, primary := s.scope(ctx)
	key := storage.TenantFromContext(scoped) + "\x00" + storage.SessionFromContext(scoped)
	s.mu.Lock()
	if err := s.plans.Save(scoped, plan); err != nil {
		s.mu.Unlock()
		return err
	}
	s.loaded[key] = true
	if s.owners == nil {
		s.owners = make(map[string]string)
	}
	if runtime, ok := taskRuntimeFromContext(ctx); ok {
		s.owners[key] = string(runtime.taskID)
	} else {
		delete(s.owners, key)
	}
	s.mu.Unlock()
	if primary && s.durable != nil {
		_ = s.durable.Save(scoped, plan)
	}
	return nil
}

func (s *taskPlanStore) load(ctx context.Context, primary bool) (*builtins.Plan, error) {
	if !primary || s.durable == nil {
		return s.plans.Load(ctx)
	}
	key := storage.TenantFromContext(ctx) + "\x00" + storage.SessionFromContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded[key] {
		return s.plans.Load(ctx)
	}
	s.loaded[key] = true
	restored, err := s.durable.Load(ctx)
	if err != nil || restored == nil || len(restored.Tasks) == 0 {
		return s.plans.Load(ctx)
	}
	if err := s.plans.Save(ctx, restored); err != nil {
		return nil, err
	}
	return restored.Clone(), nil
}

// markLoaded records that ctx's session has an in-process plan and reports
// whether it already had one.
func (s *taskPlanStore) markLoaded(ctx context.Context) bool {
	key := storage.TenantFromContext(ctx) + "\x00" + storage.SessionFromContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded[key] {
		return true
	}
	s.loaded[key] = true
	return false
}

// openItems returns the unfinished tasks of the primary plan for sessionID.
func (s *taskPlanStore) openItems(ctx context.Context, sessionID string) []builtins.PlanTask {
	if s == nil || sessionID == "" {
		return nil
	}
	plan, err := s.load(storage.WithSession(ctx, sessionID), true)
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

func (s *taskPlanStore) belongsToTask(ctx context.Context) bool {
	if s == nil {
		return false
	}
	runtime, ok := taskRuntimeFromContext(ctx)
	if !ok {
		return false
	}
	scoped, _ := s.scope(ctx)
	key := storage.TenantFromContext(scoped) + "\x00" + storage.SessionFromContext(scoped)
	s.mu.Lock()
	owner := s.owners[key]
	s.mu.Unlock()
	return owner != "" && owner == string(runtime.taskID)
}

func (s *taskPlanStore) planForTask(ctx context.Context) (*builtins.Plan, bool) {
	if s == nil {
		return nil, false
	}
	if _, err := s.Load(ctx); err != nil {
		return nil, false
	}
	scoped, _ := s.scope(ctx)
	key := storage.TenantFromContext(scoped) + "\x00" + storage.SessionFromContext(scoped)
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, err := s.plans.Load(scoped)
	if err != nil {
		return nil, false
	}
	runtime, ok := taskRuntimeFromContext(ctx)
	owned := ok && s.owners[key] != "" && s.owners[key] == string(runtime.taskID)
	return plan, owned
}

func (s *taskPlanStore) openItemsForTask(ctx context.Context, sessionID string) []builtins.PlanTask {
	plan, owned := s.planForTask(storage.WithSession(ctx, sessionID))
	if !owned || plan == nil {
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
	modelID := req.Model
	if provider, ok := evt.Metadata["provider"].(model.Provider); modelID == "" && ok && provider != nil {
		modelID = provider.Model()
	}
	if model.AnthropicThinkingBoundToConversation(modelID) {
		// A pin rebuilt on every call edits the previous request's last
		// message, which invalidates every later thinking block on these
		// models. Each plan change is already appended to the conversation
		// as the update_plan result, so the history stays append-only.
		return nil
	}
	plan, owned := h.store.planForTask(ctx)
	if plan == nil {
		return nil
	}
	// The SDK carries a round's request messages into the next round, so drop
	// the previous pin instead of stacking one per call.
	messages := make([]model.Message, 0, len(req.Messages)+1)
	for _, message := range req.Messages {
		if message.Role == model.RoleSystem && (strings.HasPrefix(message.Content, taskPlanHeader) || strings.HasPrefix(message.Content, priorTaskPlanHeader)) {
			continue
		}
		messages = append(messages, message)
	}
	if len(plan.Tasks) > 0 {
		// The plan changes during a turn; as an uncached trailing message it
		// never invalidates the provider's cached prompt and conversation.
		header := taskPlanHeader
		if _, active := taskRuntimeFromContext(ctx); active && !owned {
			header = priorTaskPlanHeader
		}
		messages = append(messages, model.Message{Role: model.RoleSystem, Content: header + plan.Summary(), Uncached: true})
	}
	req.Messages = messages
	return nil
}

func (taskPlanHook) After(context.Context, *hooks.Event) error { return nil }
