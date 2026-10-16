package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
	storagememory "github.com/spawn08/chronos/storage/adapters/memory"
)

func TestTaskContractConservativeRequestHints(t *testing.T) {
	for _, test := range []struct {
		message, role string
		mode          TaskMode
	}{
		{"build the site", "chronos-code", TaskAction},
		{"Can you fix the parser?", "coder", TaskAction},
		{"I want you to fix the parser", "coder", TaskAction},
		{"Can you compare these agents?", "chronos-code", TaskAnswer},
		{"Please write an implementation plan", "coder", TaskAnswer},
		{"Build a plan without changing files", "coder", TaskAnswer},
		{"ok go ahead!!", "chronos-code", TaskAction},
		{"Review the migration; do not apply it", "chronos-code", TaskAnswer},
		{"How do I fix this crash?", "coder", TaskAnswer},
		{"Plan the implementation", "chronos-code", TaskAnswer},
		{"Compare the agents", "chronos-code", TaskAnswer},
		{"anything pending?", "coder", TaskAnswer},
		{"fix the parser", "debugger", TaskAnswer},
		{"review the change", "reviewer", TaskAnswer},
		{"a short ambiguous followup", "chronos-code", TaskAnswer},
	} {
		t.Run(test.message, func(t *testing.T) {
			contract, err := resolveTaskContract(ExecutionRequest{Message: test.message}, test.role, false)
			if err != nil || contract.Mode != test.mode || contract.Outcome != test.message {
				t.Fatalf("contract=%#v err=%v", contract, err)
			}
		})
	}
	original := TaskContract{Mode: TaskAction, NonGoals: []string{"preserve the API"}, Acceptance: []string{"targeted test passes"}}
	contract, err := resolveTaskContract(ExecutionRequest{Message: "apply it", Contract: &original}, "coder", false)
	if err != nil {
		t.Fatal(err)
	}
	contract.NonGoals[0] = "mutated"
	if original.NonGoals[0] != "preserve the API" {
		t.Fatal("contract mutated caller state")
	}
	if _, err := parseContractPolicy([]byte("action_patterns: ['[']")); err == nil {
		t.Fatal("accepted invalid policy")
	}
}

func TestReadOnlyAndPendingDecisionSkipContinuationAndRepair(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		for _, test := range []struct {
			message, reply string
			contract       *TaskContract
		}{
			{"Review the migration; do not apply it", "The review is complete. Next step is applying it after approval.", nil},
			{"Plan the change", "The plan is ready. Should I proceed with the migration?", nil},
			{"How do I fix this crash?", "Next, I'll change the handler.", nil},
			{"apply the migration", "I need your approval before applying the destructive migration.", nil},
			{"build the feature", "Shall I proceed?", &TaskContract{Mode: TaskAction, PendingDecision: "approve external publishing"}},
		} {
			t.Run(fmt.Sprint(mode)+"/"+test.message, func(t *testing.T) {
				provider := &outputLimitedProvider{responses: []*model.ChatResponse{{Role: model.RoleAssistant, Content: test.reply, StopReason: model.StopReasonEnd}, {Role: model.RoleAssistant, Content: "must not continue", StopReason: model.StopReasonEnd}}}
				a := newExecutionTestAgent("coder", provider)
				a.Storage = storagememory.New()
				orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", taskPlans: newTaskPlanStore()}
				t.Cleanup(func() { _ = orch.Close() })
				request := ExecutionRequest{Message: test.message, SessionID: "contract-session", Mode: mode, Contract: test.contract, VerificationMode: verification.ModeReport, VerificationObligations: []verification.Obligation{{ID: "explicit-check", Kind: verification.KindTest, CommandClass: execution.CommandTest, Paths: []string{"main.go"}}}}
				response, err := executeTestTurn(context.Background(), orch, request)
				if err != nil {
					t.Fatal(err)
				}
				if provider.calls != 1 || !strings.Contains(response.Content, test.reply) {
					t.Fatalf("calls=%d response=%#v", provider.calls, response)
				}
			})
		}
	}
}

func TestPriorTaskPlanCannotDriveNewRequest(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			provider := &outputLimitedProvider{responses: []*model.ChatResponse{{Role: model.RoleAssistant, Content: "The requested typo is fixed.", StopReason: model.StopReasonEnd}, {Role: model.RoleAssistant, Content: "must not continue", StopReason: model.StopReasonEnd}}}
			a := newExecutionTestAgent("coder", provider)
			a.Storage = storagememory.New()
			plans := newTaskPlanStore()
			previous, err := newTaskRuntime("previous-task", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := withTaskRuntime(storage.WithSession(context.Background(), "same-session"), previous)
			if err := plans.Save(ctx, &builtins.Plan{Tasks: []builtins.PlanTask{{Content: "publish unrelated migration", Status: builtins.TaskInProgress}}}); err != nil {
				t.Fatal(err)
			}
			orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", taskPlans: plans}
			t.Cleanup(func() { _ = orch.Close() })
			if _, err := executeTestTurn(context.Background(), orch, ExecutionRequest{Message: "fix a typo", TaskID: "new-task", SessionID: "same-session", Mode: mode}); err != nil {
				t.Fatal(err)
			}
			if provider.calls != 1 {
				t.Fatalf("old plan drove %d calls", provider.calls)
			}
			if len(plans.openItems(ctx, "same-session")) != 1 {
				t.Fatal("prior plan was discarded")
			}
		})
	}
}

func TestReadOnlyEnforceFailureReachesStreamAndCompletion(t *testing.T) {
	provider := &outputLimitedProvider{responses: []*model.ChatResponse{{Role: model.RoleAssistant, Content: "Review complete", StopReason: model.StopReasonEnd}}}
	a := newExecutionTestAgent("coder", provider)
	a.Storage = storagememory.New()
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", taskPlans: newTaskPlanStore()}
	t.Cleanup(func() { _ = orch.Close() })
	result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "review the change", Mode: ExecutionStreaming, SessionID: "enforced-review", VerificationMode: verification.ModeEnforce, VerificationObligations: []verification.Obligation{{ID: "required-test", Kind: verification.KindTest, CommandClass: execution.CommandTest, Paths: []string{"main.go"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var terminal *execution.TerminalError
	for response := range result.Stream {
		if response != nil && response.Err != nil {
			errors.As(response.Err, &terminal)
		}
	}
	completion := <-result.Completion
	if terminal == nil || completion.Err == nil || completion.StopReason != execution.StopVerificationFailed || provider.calls != 1 {
		t.Fatalf("terminal=%v completion=%#v calls=%d", terminal, completion, provider.calls)
	}
}
