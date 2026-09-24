package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
)

type parkedDeliveryExecutor struct{}

func (parkedDeliveryExecutor) Execute(context.Context, *execution.Execution) execution.Outcome {
	return execution.Outcome{Kind: execution.OutcomeWaiting, WaitState: execution.DeliveryWaitingDecision, Signal: "verification"}
}

func TestServerStartsAndStopsOptionalDeliveryWorker(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	if _, err := store.AdmitRunnable(ctx, execution.Admission{
		Scope: scope, DeliveryID: "delivery", AdmissionKey: "request",
		Goal:  execution.Goal{Statement: "inspect", Actor: "operator"},
		Event: execution.EventIdentity{ID: "admit", IdempotencyKey: "admit-key"},
	}); err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(store, parkedDeliveryExecutor{}, execution.WorkerConfig{
		OwnerID: "server-worker", Concurrency: 1,
		LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(nil, ServerConfig{AuthType: "none", Listen: "127.0.0.1:0", DeliveryWorker: worker})
	done := make(chan error, 1)
	go func() { done <- srv.Start() }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		<-done
	}()
	deadline := time.After(3 * time.Second)
	for {
		current, err := store.Load(ctx, scope, "delivery")
		if err != nil {
			t.Fatal(err)
		}
		if current.State == execution.DeliveryWaitingDecision {
			break
		}
		select {
		case <-deadline:
			t.Fatal("server did not start the delivery worker")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
