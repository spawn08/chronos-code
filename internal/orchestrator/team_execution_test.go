package orchestrator

import (
	"context"
	"testing"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/team"

	"github.com/spawn08/chronos-code/internal/authorization"
)

func TestRunTeamBindsTaskAndPreservesInheritedWorkerAuthority(t *testing.T) {
	var contexts []context.Context
	provider := resourceProvider{chat: func(ctx context.Context, _ *model.ChatRequest) (*model.ChatResponse, error) {
		contexts = append(contexts, ctx)
		return resourceReply("done"), nil
	}}
	members := team.New("inspect", "Inspect", team.StrategySequential).
		AddAgent(resourceAgent(t, "one", provider)).AddAgent(resourceAgent(t, "two", provider))
	orch := &Orchestrator{teams: map[string]*team.Team{"inspect": members}, active: "one", sessions: map[string]string{"one": "session"}}
	request := authorization.Request{PrincipalID: "worker", TenantID: "tenant", RepositoryID: "repo", Action: "delivery.execute"}
	ctx := authorization.WithRequest(context.Background(), request)
	ctx = tool.WithEffectGrant(ctx, tool.EffectRead)
	result, err := orch.RunTeam(ctx, "inspect", "report evidence")
	if err != nil || result == "" || len(contexts) != 2 {
		t.Fatalf("RunTeam() = %q, %v; provider calls = %d", result, err, len(contexts))
	}
	first, ok := agent.RunIdentityFromContext(contexts[0])
	if !ok || first.TenantID != "tenant" || first.RepositoryID != "repo" || first.TaskID == "" || first.RoleID != "team:inspect" || first.InvocationID == "" {
		t.Fatalf("team identity = %+v, ok=%v", first, ok)
	}
	second, ok := agent.RunIdentityFromContext(contexts[1])
	if !ok || second.TaskID != first.TaskID {
		t.Fatalf("second member task identity = %+v, ok=%v", second, ok)
	}
	initial, ok := taskRuntimeFromContext(contexts[0])
	if !ok {
		t.Fatal("team has no task runtime")
	}
	if inherited, ok := taskRuntimeFromContext(contexts[1]); !ok || inherited != initial {
		t.Fatal("team members do not share task runtime")
	}
	for _, memberCtx := range contexts {
		grant, ok := tool.EffectGrantFromContext(memberCtx)
		if !ok || len(grant) != 1 {
			t.Fatalf("team effect grant = %v, ok=%v", grant, ok)
		}
		if _, ok := grant[tool.EffectRead]; !ok {
			t.Fatalf("team lost read grant: %v", grant)
		}
	}

	contexts = nil
	worker := agent.RunIdentity{TenantID: "tenant", RepositoryID: "repo", DeliveryID: "delivery", TaskID: "delivery", RoleID: "worker", InvocationID: "worker-run"}
	ctx = agent.WithRunIdentity(withTaskRuntime(ctx, initial), worker)
	if _, err := orch.RunTeam(ctx, "inspect", "inspect again"); err != nil {
		t.Fatal(err)
	}
	if len(contexts) != 2 {
		t.Fatalf("inherited worker provider calls = %d", len(contexts))
	}
	for _, memberCtx := range contexts {
		identity, ok := agent.RunIdentityFromContext(memberCtx)
		if !ok || identity != worker {
			t.Fatalf("inherited worker identity = %+v, ok=%v", identity, ok)
		}
		if runtime, ok := taskRuntimeFromContext(memberCtx); !ok || runtime != initial {
			t.Fatal("inherited worker runtime changed")
		}
	}
}
