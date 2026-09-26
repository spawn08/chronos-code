package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/workspace"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

func TestObservedHTTPDestinationReconcilesRestartWithoutSecondMutation(t *testing.T) {
	clock := &deliveryClock{}
	clock.timestamp.Store(time.Now().Add(time.Second).UnixNano())
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store, err := execution.OpenDeliveryStoreWithClock(context.Background(), path, clock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	receipts := make(map[string]any)
	mutations := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/effects/") {
			mu.Lock()
			value, ok := receipts[strings.TrimPrefix(r.URL.Path, "/effects/")]
			mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(value)
			return
		}
		if r.URL.Path != "/mutate" || r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") == "" || r.Header.Get("Idempotency-Key") == "forged" {
			http.Error(w, "invalid mutation", http.StatusBadRequest)
			return
		}
		mu.Lock()
		mutations++
		mu.Unlock()
		w.Header().Set("Date", "Mon, 01 Jan 2024 00:00:00 GMT")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
		mu.Lock()
		receipts[r.Header.Get("Idempotency-Key")] = map[string]any{"status_code": http.StatusCreated, "headers": map[string]string{
			"Date": "Mon, 01 Jan 2024 00:00:00 GMT", "Content-Type": "text/plain; charset=utf-8", "Content-Length": "2",
		}, "body": "ok"}
		mu.Unlock()
	}))
	defer remote.Close()
	definition, err := newDeliveryHTTPTool(config.DeliveryHTTPConfig{RequestURL: remote.URL + "/mutate", ObservationURL: remote.URL + "/effects"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := definition.Recovery.Prepare(context.Background(), map[string]any{"method": "POST", "url": "https://other.example/"}, "key"); err == nil {
		t.Fatal("model-supplied destination overrode configured endpoint")
	}
	if _, err := definition.Handler(context.Background(), map[string]any{"method": "POST"}); err == nil {
		t.Fatal("mutation sent without host-issued idempotency key")
	}
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(context.Background(), execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", Goal: execution.Goal{Statement: "create item", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(context.Background(), "old", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	providerCalls := 0
	provider := resourceProvider{chat: func(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
		providerCalls++
		if providerCalls == 1 {
			return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: "call-1", Name: definition.Name, Arguments: `{"method":"POST","headers":{"Idempotency-Key":"forged"}}`}}}, nil
		}
		return resourceReply("done"), nil
	}}
	writer := newExecutionTestAgent("writer", provider)
	definition.Permission = tool.PermAllow // fixture supplies the worker's effect grant.
	original := definition.Handler
	definition.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		result, err := original(ctx, args)
		if err != nil {
			return nil, err
		}
		return result, store.Close() // remote committed; host died before receipt.
	}
	writer.Tools.Register(definition)
	wrapDeliveryOperations(writer)
	ctx := security.WithEffectGrant(context.Background(), security.EffectNetwork, security.EffectExternalMutation)
	ctx = agent.WithRunIdentity(execution.WithOperationLease(ctx, store, lease), agent.RunIdentity{TaskID: "delivery", RoleID: "writer", InvocationID: "old-invocation"})
	if _, err := writer.Chat(ctx, "create"); err == nil || providerCalls != 1 {
		t.Fatalf("uncheckpointed mutation = calls %d, error %v", providerCalls, err)
	}
	clock.timestamp.Add(int64(time.Minute + time.Second))
	store, err = execution.OpenDeliveryStoreWithClock(context.Background(), path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reader := &executionTestProvider{name: "reader", modelID: "fixture"}
	orch := &Orchestrator{agents: map[string]*agent.Agent{"writer": writer, "reader": newExecutionTestAgent("reader", reader)}, active: "reader", workspace: &workspace.Info{Root: t.TempDir()}}
	executor, err := NewReadOnlyDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(store, executor, execution.WorkerConfig{OwnerID: "replacement", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, err := store.Operation(context.Background(), scope, "delivery", "old-invocation:call-1")
	mu.Lock()
	count := mutations
	mu.Unlock()
	if err != nil || op.Status != execution.OperationReconciled || op.ReplayClass != execution.ReplayIdempotentExternal || count != 1 || len(reader.contexts) != 0 {
		t.Fatalf("observed HTTP recovery: operation=%+v, mutations=%d, reader calls=%d, error=%v", op, count, len(reader.contexts), err)
	}
	if _, err := store.ReconcileOperation(context.Background(), lease, op.ID, "late", op.OutputFingerprint, op.Result); !errors.Is(err, execution.ErrStaleLease) {
		t.Fatalf("stale worker reconciled HTTP effect: %v", err)
	}
}

func TestObservedHTTPDestinationRequiresTrustedSameOrigin(t *testing.T) {
	for _, urls := range []config.DeliveryHTTPConfig{
		{RequestURL: "https://api.example.com/mutate", ObservationURL: "https://other.example.com/effects"},
		{RequestURL: "https://api.example.com/mutate?token=secret", ObservationURL: "https://api.example.com/effects"},
		{RequestURL: "http://api.example.com/mutate", ObservationURL: "http://api.example.com/effects"},
	} {
		if _, err := newDeliveryHTTPTool(urls); err == nil {
			t.Fatalf("untrusted HTTP destination accepted: %+v", urls)
		}
	}
}

func TestConfiguredHTTPObserverIsInstalledBeforeDeliveryJournalWrapper(t *testing.T) {
	t.Setenv("CHRONOS_CODE_DATA_HOME", t.TempDir())
	root := t.TempDir()
	index := false
	cfg := &config.Config{FileConfig: agent.FileConfig{
		Defaults: &agent.AgentConfig{Storage: agent.StorageConfig{Backend: "sqlite", DSN: filepath.Join(root, "sessions.db")}},
		Agents:   []agent.AgentConfig{{ID: "writer", Name: "Writer", Model: agent.ModelConfig{Provider: "openai", Model: "gpt-4o-mini", APIKey: "test-key"}}},
	}, Workspace: config.WorkspaceConfig{Root: root, IndexOnStart: &index}}
	cfg.Server.DeliveryHTTP = config.DeliveryHTTPConfig{RequestURL: "https://api.example.test/mutate", ObservationURL: "https://api.example.test/effects"}
	orch, err := New(context.Background(), cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer orch.Close()
	definition, ok := orch.agents["writer"].Tools.Get("http_observed_mutation")
	if !ok || definition.Recovery == nil || definition.Handler == nil {
		t.Fatalf("configured downstream effect observer = %+v", definition)
	}
}

var _ tool.EffectRecoveryAdapter = (*deliveryHTTPDestination)(nil)
