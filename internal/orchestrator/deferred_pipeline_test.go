package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
)

func TestDeferredPipelineJournalsOnlyTargetEffect(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "delivery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", Goal: execution.Goal{Statement: "write output", Actor: "user"}, Event: execution.EventIdentity{ID: "event", IdempotencyKey: "event-key"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	calls, writes := 0, 0
	provider := resourceProvider{chat: func(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
		calls++
		if calls%2 == 1 {
			return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: "dispatch-1", Name: "call_tool", Arguments: `{"name":"file_write","arguments":{"path":"output","content":"done"}}`}}}, nil
		}
		return resourceReply("finished"), nil
	}}
	a := newExecutionTestAgent("worker", provider)
	a.Storage = pipelineAgent(t).Storage
	a.Tools.Register(&tool.Definition{Name: "file_write", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectDeliveryWrite}, Handler: func(_ context.Context, args map[string]any) (any, error) {
		writes++
		if err := os.WriteFile(filepath.Join(root, "output"), []byte(args["content"].(string)), 0600); err != nil {
			return nil, err
		}
		return map[string]any{"path": "output"}, nil
	}})
	o := &Orchestrator{cfg: &config.Config{Tools: config.ToolsConfig{DeferredSchemas: true}}}
	o.installDeferredTools(a)
	wrapToolPipeline(a, nil, config.HooksConfig{}, nil, nil)
	ctx = builtins.WithWorkspaceRoot(ctx, root)
	ctx = agent.WithRunIdentity(ctx, agent.RunIdentity{TaskID: "delivery", RoleID: "worker", InvocationID: "invocation"})
	ctx = tool.WithEffectGrant(ctx, tool.EffectRead, tool.EffectDeliveryWrite)
	ctx = execution.WithOperationLease(ctx, store, lease)
	for range 2 {
		response, err := a.Chat(ctx, "write output once")
		if err != nil || response.Content != "finished" {
			t.Fatalf("dispatch failed: response=%v error=%v", response, err)
		}
	}
	if writes != 1 {
		t.Fatalf("effect executions=%d", writes)
	}
	operation, err := store.Operation(ctx, scope, "delivery", "invocation:dispatch-1")
	if err != nil || operation.Kind != "file_write" || operation.Status != execution.OperationObserved {
		t.Fatalf("incorrect target journal: %+v error=%v", operation, err)
	}
}
