package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/workspace"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	memorystore "github.com/spawn08/chronos/storage/adapters/memory"
)

func TestWorkerRestartReusesCompletedProviderReply(t *testing.T) {
	for _, session := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "session"}[session], func(t *testing.T) {
			ctx := context.Background()
			clock := &toolRoundCrashClock{}
			clock.timestamp.Store(time.Now().Add(time.Second).UnixNano())
			path := filepath.Join(t.TempDir(), "delivery.db")
			store, err := execution.OpenDeliveryStoreWithClock(ctx, path, clock)
			if err != nil {
				t.Fatal(err)
			}
			scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
			if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "request", Goal: execution.Goal{Statement: "inspect", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
				t.Fatal(err)
			}
			lease, err := store.Claim(ctx, "old", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			provider := toolRoundCrashProvider{chat: func(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
				calls++
				return &model.ChatResponse{Content: "saved reply", StopReason: model.StopReasonEnd, Usage: model.Usage{PromptTokens: 3, CompletionTokens: 2}, UsageKnown: true}, nil
			}}
			builder := agent.New("reader", "Reader").WithModel(provider)
			var sessionStore *memorystore.Store
			if session {
				sessionStore = memorystore.New()
				builder.WithStorage(sessionStore)
			}
			a, err := builder.Build()
			if err != nil {
				t.Fatal(err)
			}
			a.Hooks = append(a.Hooks, deliveryBudgetHook{session: budgetHook{tracker: budget.NewTracker(0, 0), agentID: "reader"}})
			roles := map[string]*agent.Agent{"reader": a}
			first := agent.WithRunIdentity(execution.WithOperationLease(ctx, store, lease), agent.RunIdentity{TaskID: "delivery", RoleID: "reader", InvocationID: "first"})
			first = agent.WithToolRoundJournal(first, deliveryToolRoundJournal{attempt: &execution.Execution{Lease: lease}, roles: roles})
			var reply *model.ChatResponse
			if session {
				reply, err = a.ChatWithSession(first, "delivery:delivery", "inspect")
			} else {
				reply, err = a.Chat(first, "inspect")
			}
			if err != nil || reply.Content != "saved reply" || calls != 1 {
				t.Fatalf("original reply=%+v calls=%d err=%v", reply, calls, err)
			}
			var eventCount int
			if session {
				events, err := sessionStore.ListEvents(ctx, "delivery:delivery", 0)
				if err != nil {
					t.Fatal(err)
				}
				eventCount = len(events)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			clock.timestamp.Add(int64(time.Minute + time.Second))
			store, err = execution.OpenDeliveryStoreWithClock(ctx, path, clock)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			orch := &Orchestrator{agents: roles, active: "reader", workspace: &workspace.Info{Root: t.TempDir()}}
			executor, err := NewReadOnlyDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
			if err != nil {
				t.Fatal(err)
			}
			worker, err := execution.NewWorker(store, executor, execution.WorkerConfig{OwnerID: "replacement", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatalf("replacement worker: %v", err)
			}
			if calls != 1 {
				t.Fatalf("replacement resubmitted billed provider call: %d", calls)
			}
			usage, err := store.Usage(ctx, scope, "delivery")
			if err != nil || usage.ReconciledCalls != 1 {
				t.Fatalf("provider usage=%+v err=%v", usage, err)
			}
			attempts, err := store.Attempts(ctx, scope, "delivery")
			if err != nil || len(attempts) != 2 || !json.Valid(attempts[1].Result) {
				t.Fatalf("recovered result=%+v err=%v", attempts, err)
			}
			var result struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(attempts[1].Result, &result); err != nil || result.Content != "saved reply" {
				t.Fatalf("recovered result=%+v err=%v", result, err)
			}
			if session {
				events, err := sessionStore.ListEvents(ctx, "delivery:delivery", 0)
				if err != nil || len(events) != eventCount {
					t.Fatalf("replayed session wrote duplicate messages: events=%d before=%d err=%v", len(events), eventCount, err)
				}
			}
		})
	}
}

func TestWorkerRestartParksUnknownProviderReply(t *testing.T) {
	ctx := context.Background()
	clock := &toolRoundCrashClock{}
	clock.timestamp.Store(time.Now().Add(time.Second).UnixNano())
	path := filepath.Join(t.TempDir(), "delivery.db")
	store, err := execution.OpenDeliveryStoreWithClock(ctx, path, clock)
	if err != nil {
		t.Fatal(err)
	}
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{Scope: scope, DeliveryID: "delivery", AdmissionKey: "key", Goal: execution.Goal{Statement: "inspect", Actor: "user"}, Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "old", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	provider := toolRoundCrashProvider{chat: func(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
		calls++
		return &model.ChatResponse{Content: "unknown billing", StopReason: model.StopReasonEnd}, nil
	}}
	a, err := agent.New("reader", "Reader").WithModel(provider).Build()
	if err != nil {
		t.Fatal(err)
	}
	a.Hooks = append(a.Hooks, deliveryBudgetHook{session: budgetHook{tracker: budget.NewTracker(0, 0), agentID: "reader"}})
	roles := map[string]*agent.Agent{"reader": a}
	first := agent.WithRunIdentity(execution.WithOperationLease(ctx, store, lease), agent.RunIdentity{TaskID: "delivery", RoleID: "reader", InvocationID: "first"})
	first = agent.WithToolRoundJournal(first, deliveryToolRoundJournal{attempt: &execution.Execution{Lease: lease}, roles: roles})
	if _, err := a.Chat(first, "inspect"); !errors.Is(err, execution.ErrUsageOutcomeUnknown) {
		t.Fatalf("unknown provider response was checkpointed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	clock.timestamp.Add(int64(time.Minute + time.Second))
	store, err = execution.OpenDeliveryStoreWithClock(ctx, path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	orch := &Orchestrator{agents: roles, active: "reader", workspace: &workspace.Info{Root: t.TempDir()}}
	executor, err := NewReadOnlyDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(store, executor, execution.WorkerConfig{OwnerID: "replacement", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil || calls != 1 {
		t.Fatalf("unknown call replayed: calls=%d err=%v", calls, err)
	}
	usage, err := store.Usage(ctx, scope, "delivery")
	if err != nil || usage.UnknownCalls != 1 {
		t.Fatalf("unknown usage=%+v err=%v", usage, err)
	}
}
