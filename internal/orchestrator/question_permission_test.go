package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

func TestQuestionPermissionMetadataDoesNotPromptForSeparateApproval(t *testing.T) {
	o := &Orchestrator{}
	a := &agent.Agent{ID: "reader", Tools: tool.NewRegistry()}
	o.agents = map[string]*agent.Agent{a.ID: a}
	installUserQuestion(o.agents, o)
	normalizeToolPermissions(o.agents)
	definition, _ := a.Tools.Get(AskUserQuestionToolName)
	if definition.Permission != tool.PermRequireApproval {
		t.Fatal("expected normalized internal permission routing")
	}
	approvals, questions := 0, 0
	o.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) { approvals++; return false, nil })
	o.SetUserQuestionHandler(func(context.Context, []UserQuestion) (UserQuestionResult, error) {
		questions++
		return UserQuestionResult{Status: "answered", Answers: []UserAnswer{{Question: "Which?", Text: "Choice"}}, Message: "custom handler detail"}, nil
	})
	result, err := a.Tools.Execute(executionEffectContext(context.Background(), true), AskUserQuestionToolName, map[string]any{"questions": []UserQuestion{{Question: "Which?"}}})
	if err != nil {
		t.Fatal(err)
	}
	answer := result.(UserQuestionResult)
	if approvals != 0 || questions != 1 || answer.Status != "answered" {
		t.Fatalf("approvals=%d questions=%d result=%+v", approvals, questions, answer)
	}
	if !strings.Contains(answer.Message, "auto-allows") || !strings.Contains(answer.Message, "internal routing") || !strings.Contains(answer.Message, "custom handler detail") {
		t.Fatalf("permission facts or handler detail missing: %s", answer.Message)
	}
	denied := *definition
	denied.Permission = tool.PermDeny
	a.Tools.Register(&denied)
	if _, err := a.Tools.Execute(context.Background(), AskUserQuestionToolName, map[string]any{"questions": []UserQuestion{{Question: "Which?"}}}); err == nil {
		t.Fatal("explicit denial was bypassed")
	}
	if questions != 1 {
		t.Fatal("denied tool reached interactive handler")
	}
}
