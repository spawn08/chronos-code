package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
	storagememory "github.com/spawn08/chronos/storage/adapters/memory"
)

func TestHandBackPhrase(t *testing.T) {
	for _, text := range []string{
		"**Next session:** reply \u201ccontinue\u201d and I'll write app/globals.css.",
		"Most sections are written. Next is the stylesheet (dark enterprise theme).",
		"I've mapped the call sites. Shall I apply the fix?",
		"The plan is ready. Should I proceed with the migration?",
		"Next, I'll add the tests for the parser.",
		"Now I will run the build.",
		"Let me write the remaining components.",
		"Let me know if you want me to proceed.",
		"This needs a new session to finish.",
	} {
		if handBackPhrase(text) == "" {
			t.Errorf("handBackPhrase(%q) = \"\", want a match", text)
		}
	}
	for _, text := range []string{
		"All three endpoints are implemented and `go test ./...` passes.",
		"Done. If you'd like, I can also add caching later.",
		"Let me know if you'd like any changes to the copy.",
		"The next step for production is configuring the CRM webhook (needs your API key).",
		"",
	} {
		if phrase := handBackPhrase(text); phrase != "" {
			t.Errorf("handBackPhrase(%q) = %q, want no match", text, phrase)
		}
	}
}

func TestExecuteNudgesHandBackUntilTextOnlyReply(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionBlocking, ExecutionStreaming} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			provider := &outputLimitedProvider{responses: []*model.ChatResponse{
				{Role: model.RoleAssistant, Content: "Scaffolded the pages. Next is the stylesheet.", StopReason: model.StopReasonEnd},
				{Role: model.RoleAssistant, Content: "Shall I proceed?", StopReason: model.StopReasonEnd},
				{Role: model.RoleAssistant, Content: "must not be requested", StopReason: model.StopReasonEnd},
			}}
			a := newExecutionTestAgent("coder", provider)
			a.Storage = storagememory.New()
			orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", taskPlans: newTaskPlanStore()}
			t.Cleanup(func() { _ = orch.Close() })

			response, err := executeTestTurn(context.Background(), orch, ExecutionRequest{Message: "build the site", SessionID: "gate-session", Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			// The reply to the nudge made no tool calls, so the turn ends there.
			if provider.calls != 2 {
				t.Fatalf("model calls=%d, want 2", provider.calls)
			}
			nudge := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
			if nudge.Role != model.RoleUser || !strings.Contains(nudge.Content, "next is") || !strings.Contains(nudge.Content, "not watching") {
				t.Fatalf("nudge message=%+v", nudge)
			}
			if !strings.Contains(response.Content, "Next is the stylesheet") || !strings.Contains(response.Content, "Shall I proceed?") {
				t.Fatalf("response content=%q, want both replies", response.Content)
			}
		})
	}
}

func TestExecuteNudgesOpenPlanItems(t *testing.T) {
	provider := &outputLimitedProvider{responses: []*model.ChatResponse{
		{Role: model.RoleAssistant, Content: "Finished the layout.", StopReason: model.StopReasonEnd},
		{Role: model.RoleAssistant, Content: "Styles are done too.", StopReason: model.StopReasonEnd},
	}}
	a := newExecutionTestAgent("coder", provider)
	a.Storage = storagememory.New()
	plans := newTaskPlanStore()
	ctx := storage.WithSession(context.Background(), "plan-session")
	if err := plans.Save(ctx, &builtins.Plan{Tasks: []builtins.PlanTask{
		{Content: "layout", Status: builtins.TaskCompleted},
		{Content: "write globals.css", Status: builtins.TaskInProgress},
	}}); err != nil {
		t.Fatal(err)
	}
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder", taskPlans: plans}
	t.Cleanup(func() { _ = orch.Close() })

	if _, err := executeTestTurn(context.Background(), orch, ExecutionRequest{Message: "build the site", SessionID: "plan-session"}); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 2 {
		t.Fatalf("model calls=%d, want 2", provider.calls)
	}
	nudge := provider.requests[1].Messages[len(provider.requests[1].Messages)-1].Content
	if !strings.Contains(nudge, "[in_progress] write globals.css") || strings.Contains(nudge, "] layout") {
		t.Fatalf("nudge=%q, want only the open item", nudge)
	}
}

func TestCompletionNudgeBounds(t *testing.T) {
	a := &agent.Agent{Storage: storagememory.New()}
	orch := &Orchestrator{taskPlans: newTaskPlanStore()}
	ctx := context.Background()
	handBack := "Next, I'll write the tests."

	gate := &completionGate{}
	for calls := 1; calls <= maxCompletionNudges; calls++ {
		if orch.completionNudge(ctx, a, "s", gate, model.StopReasonEnd, handBack, calls) == "" {
			t.Fatalf("nudge %d was not sent", calls)
		}
	}
	if orch.completionNudge(ctx, a, "s", gate, model.StopReasonEnd, handBack, 99) != "" {
		t.Fatal("nudged past the cap")
	}
	if orch.completionNudge(ctx, a, "s", &completionGate{}, model.StopReasonPaused, handBack, 1) != "" {
		t.Fatal("nudged a paused turn")
	}
	if orch.completionNudge(ctx, a, "", &completionGate{}, model.StopReasonEnd, handBack, 1) != "" {
		t.Fatal("nudged without a session to continue")
	}
	orch.SetPlanMode(true)
	if orch.completionNudge(ctx, a, "s", &completionGate{}, model.StopReasonEnd, handBack, 1) != "" {
		t.Fatal("nudged in plan mode")
	}
}

func TestExecuteSubagentStreamReturnsFinalMessage(t *testing.T) {
	provider := &outputLimitedProvider{responses: []*model.ChatResponse{
		{Role: model.RoleAssistant, Content: "STATUS: complete", StopReason: model.StopReasonEnd},
	}}
	result, err := executeSubagentStream(context.Background(), newExecutionTestAgent("coder", provider), "write the file")
	if err != nil || result != "STATUS: complete" {
		t.Fatalf("executeSubagentStream = %q, %v", result, err)
	}
}

func TestEnvironmentLineReportsGitRepository(t *testing.T) {
	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	root := t.TempDir()
	if line := environmentLine(root, now); !strings.Contains(line, "date=2026-09-27") || !strings.Contains(line, "git_repository=false") {
		t.Fatalf("environmentLine = %q", line)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if line := environmentLine(root, now); !strings.Contains(line, "git_repository=true") {
		t.Fatalf("environmentLine = %q", line)
	}
}

func TestWorkspacePinNamesTheRequestModel(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/x\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &agent.Agent{ID: "chronos-code", Model: &routingTestProvider{provider: "anthropic", model: "claude-sonnet-5"}, Tools: tool.NewRegistry()}
	if setupWorkspace(root, map[string]*agent.Agent{a.ID: a}) == nil {
		t.Fatal("workspace not detected")
	}
	pins := func(ctx context.Context) string { return strings.Join(messageContents(a.ContextPinsFn(ctx)), "\n") }
	if got := pins(context.Background()); !strings.Contains(got, "served by claude-sonnet-5 (provider anthropic)") {
		t.Fatalf("pins without a routed model = %q, want the agent's model", got)
	}
	routed := agent.WithModelProvider(context.Background(), &routingTestProvider{provider: "anthropic", model: "claude-opus-5-5"})
	if got := pins(routed); !strings.Contains(got, "served by claude-opus-5-5 (provider anthropic)") || strings.Contains(got, "claude-sonnet-5") {
		t.Fatalf("pins with a routed model = %q, want the routed model only", got)
	}
}
