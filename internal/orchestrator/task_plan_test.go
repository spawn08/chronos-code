package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

func TestTaskPlanHookPinsCurrentPlanOnce(t *testing.T) {
	store := newTaskPlanStore()
	ctx := storage.WithSession(context.Background(), "s")
	hook := taskPlanHook{store: store}
	req := &model.ChatRequest{Messages: []model.Message{
		{Role: model.RoleSystem, Content: "system prompt"},
		{Role: model.RoleUser, Content: "task"},
	}}
	call := func() {
		t.Helper()
		if err := hook.Before(ctx, &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
			t.Fatal(err)
		}
	}
	pins := func() []string {
		var found []string
		for _, message := range req.Messages {
			if strings.HasPrefix(message.Content, taskPlanHeader) {
				found = append(found, message.Content)
			}
		}
		return found
	}

	call()
	if len(pins()) != 0 {
		t.Fatalf("pinned an empty plan: %+v", req.Messages)
	}
	if err := store.Save(ctx, &builtins.Plan{Tasks: []builtins.PlanTask{{Content: "write css", Status: builtins.TaskPending}}}); err != nil {
		t.Fatal(err)
	}
	call()
	if err := store.Save(ctx, &builtins.Plan{Tasks: []builtins.PlanTask{{Content: "write css", Status: builtins.TaskCompleted}}}); err != nil {
		t.Fatal(err)
	}
	call() // the SDK carries request messages into the next round
	got := pins()
	if len(got) != 1 || !strings.Contains(got[0], "[x] 1. write css") {
		t.Fatalf("plan pins = %q, want one current pin", got)
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Content != got[0] || !last.Uncached || req.Messages[0].Content != "system prompt" || req.Messages[1].Role != model.RoleUser {
		t.Fatalf("pin not placed as the uncached trailing message after an unchanged prefix: %+v", req.Messages)
	}
	if err := store.Save(ctx, &builtins.Plan{}); err != nil {
		t.Fatal(err)
	}
	call()
	if len(pins()) != 0 {
		t.Fatalf("cleared plan still pinned: %+v", req.Messages)
	}
}

func TestTaskPlanStoreIsolatesDelegatedChildren(t *testing.T) {
	store := newTaskPlanStore()
	parent := storage.WithSession(context.Background(), "s")
	parent = agent.WithRunIdentity(parent, agent.RunIdentity{InvocationID: "parent", RoleID: "chronos-code"})
	child := agent.WithRunIdentity(parent, agent.RunIdentity{InvocationID: "child", ParentInvocationID: "parent", RoleID: "coder"})

	if err := store.Save(parent, &builtins.Plan{Tasks: []builtins.PlanTask{{Content: "parent item"}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(child, &builtins.Plan{Tasks: []builtins.PlanTask{{Content: "child item"}}}); err != nil {
		t.Fatal(err)
	}
	open := store.openItems(context.Background(), "s")
	if len(open) != 1 || open[0].Content != "parent item" {
		t.Fatalf("parent open items = %+v; a child overwrote the parent plan", open)
	}
}

func TestInstallTaskPlanRegistersOnlyForWritingAgents(t *testing.T) {
	writer := newExecutionTestAgent("coder", &outputLimitedProvider{})
	writer.Tools.Register(builtins.NewFileWriteTool(t.TempDir()))
	reader := newExecutionTestAgent("researcher", &outputLimitedProvider{})
	installTaskPlan(map[string]*agent.Agent{"coder": writer, "researcher": reader}, newTaskPlanStore(), nil)
	if _, ok := writer.Tools.Get(builtins.PlanToolName); !ok {
		t.Fatal("writing agent lacks update_plan; prompts reference it")
	}
	if _, ok := reader.Tools.Get(builtins.PlanToolName); ok {
		t.Fatal("read-only agent received update_plan")
	}
}
