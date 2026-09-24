package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/security"
)

type deliveryToolProvider struct{ calls int }

func (p *deliveryToolProvider) Name() string  { return "fixture" }
func (p *deliveryToolProvider) Model() string { return "fixture" }
func (p *deliveryToolProvider) Chat(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
	p.calls++
	if p.calls == 1 {
		return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: "call-1", Name: "file_write", Arguments: `{"path":"output","content":"done"}`}}}, nil
	}
	return &model.ChatResponse{Content: "finished", StopReason: model.StopReasonEnd}, nil
}
func (p *deliveryToolProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, errors.New("streaming not used")
}

func TestDeliveryToolEffectIsJournaledBeforeAndAfterRealAgentToolLoop(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
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
	provider := &deliveryToolProvider{}
	writes := 0
	a := newExecutionTestAgent("writer", provider)
	a.Tools.Register(&tool.Definition{Name: "file_write", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectDeliveryWrite}, Handler: func(_ context.Context, args map[string]any) (any, error) {
		writes++
		return map[string]any{"path": "output"}, os.WriteFile(filepath.Join(root, "output"), []byte(args["content"].(string)), 0o600)
	}})
	wrapDeliveryOperations(a)
	ctx = builtins.WithWorkspaceRoot(ctx, root)
	ctx = agent.WithRunIdentity(ctx, agent.RunIdentity{TaskID: "delivery", RoleID: "writer", InvocationID: "invocation-1"})
	ctx = security.WithEffectGrant(ctx, security.EffectRead, security.EffectDeliveryWrite)
	ctx = execution.WithOperationLease(ctx, store, lease)
	response, err := a.Chat(ctx, "write output")
	if err != nil || response.Content != "finished" || provider.calls != 2 {
		t.Fatalf("response = %+v, model calls = %d, error = %v", response, provider.calls, err)
	}
	op, err := store.Operation(ctx, scope, "delivery", "invocation-1:call-1")
	if err != nil || op.Status != execution.OperationObserved || op.OutputFingerprint == "" || op.ReplayClass != execution.ReplayFingerprintedWrite {
		t.Fatalf("persisted tool operation = %+v, error = %v", op, err)
	}
	events, err := store.Events(ctx, scope, "delivery")
	if err != nil || events[len(events)-1].Type != execution.DeliveryEventOperationObserved {
		t.Fatalf("tool events = %+v, error = %v", events, err)
	}
	provider.calls = 0
	if _, err := a.Chat(ctx, "try the same call again"); (!errors.Is(err, execution.ErrEffectNeedsReconciliation) && !errors.Is(err, execution.ErrOperationConflict)) || writes != 1 {
		t.Fatalf("duplicate effect: writes = %d, error = %v", writes, err)
	}
}

func TestDeliveryToolStorageFailureAfterEffectAbortsModelLoop(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store, err := execution.OpenDeliveryStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", Goal: execution.Goal{Statement: "write output", Actor: "user"}, Event: execution.EventIdentity{ID: "event", IdempotencyKey: "event-key"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	provider := &deliveryToolProvider{}
	a := newExecutionTestAgent("writer", provider)
	a.Tools.Register(&tool.Definition{Name: "file_write", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectDeliveryWrite}, Handler: func(_ context.Context, _ map[string]any) (any, error) {
		if err := os.WriteFile(filepath.Join(root, "output"), []byte("committed"), 0o600); err != nil {
			return nil, err
		}
		return map[string]any{"path": "output"}, store.Close() // simulate crash before observation persistence
	}})
	wrapDeliveryOperations(a)
	ctx = builtins.WithWorkspaceRoot(ctx, root)
	ctx = agent.WithRunIdentity(ctx, agent.RunIdentity{TaskID: "delivery", RoleID: "writer", InvocationID: "invocation-1"})
	ctx = security.WithEffectGrant(ctx, security.EffectRead, security.EffectDeliveryWrite)
	ctx = execution.WithOperationLease(ctx, store, lease)
	if _, err := a.Chat(ctx, "write"); err == nil || provider.calls != 1 {
		t.Fatalf("ambiguous effect was presented to model: calls = %d, error = %v", provider.calls, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "output")); err != nil || string(data) != "committed" {
		t.Fatalf("effect = %q, error = %v", data, err)
	}
	reopened, err := execution.OpenDeliveryStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	op, err := reopened.Operation(context.Background(), scope, "delivery", "invocation-1:call-1")
	if err != nil || op.Status != execution.OperationRunning {
		t.Fatalf("unobserved effect = %+v, error = %v", op, err)
	}
}
