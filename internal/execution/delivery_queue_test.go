package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type controlledClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *controlledClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *controlledClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type executorFunc func(context.Context, *Execution) Outcome

func (f executorFunc) Execute(ctx context.Context, execution *Execution) Outcome {
	return f(ctx, execution)
}

func TestAdmitEnqueueExecute(t *testing.T) {
	clock := newControlledClock()
	store := openQueueTestStore(t, filepath.Join(t.TempDir(), "deliveries.db"), clock)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	delivery, err := store.AdmitRunnable(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.State != DeliveryQueued || delivery.Version != 2 {
		t.Fatalf("admitted runnable delivery = %#v", delivery)
	}

	executor := executorFunc(func(ctx context.Context, execution *Execution) Outcome {
		if ctx.Err() != nil || execution.Lease.Delivery.State != DeliveryRunning {
			t.Fatalf("execution lease = %#v, context error = %v", execution.Lease, ctx.Err())
		}
		if err := execution.Checkpoint(ctx, []byte(`{"step":1}`)); err != nil {
			t.Fatal(err)
		}
		if err := execution.PublishResult(ctx, []byte(`{"preview":true}`)); err != nil {
			t.Fatal(err)
		}
		return Outcome{Kind: OutcomeSucceeded, Result: json.RawMessage(`{"ok":true}`)}
	})
	worker := newTestWorker(t, store, "worker-1", executor)
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), admission.Scope, admission.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != DeliverySucceeded {
		t.Fatalf("delivery state = %s, want %s", loaded.State, DeliverySucceeded)
	}
	attempts, err := store.Attempts(context.Background(), admission.Scope, admission.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Outcome != OutcomeSucceeded || string(attempts[0].Checkpoint) != `{"step":1}` || string(attempts[0].Result) != `{"ok":true}` {
		t.Fatalf("attempts = %#v", attempts)
	}
}

func TestWorkerRestartReclaimsExpiredLease(t *testing.T) {
	clock := newControlledClock()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store := openQueueTestStore(t, path, clock)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	if _, err := store.AdmitRunnable(context.Background(), admission); err != nil {
		t.Fatal(err)
	}
	first, err := store.Claim(context.Background(), "crashed-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute + time.Second)
	store = openQueueTestStore(t, path, clock)
	worker := newTestWorker(t, store, "replacement-worker", executorFunc(func(context.Context, *Execution) Outcome {
		return Outcome{Kind: OutcomeSucceeded}
	}))
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempts, err := store.Attempts(context.Background(), admission.Scope, admission.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].LeaseEpoch != first.Epoch || attempts[0].Outcome != "lease_expired" || attempts[1].LeaseEpoch <= first.Epoch || attempts[1].OwnerID != "replacement-worker" {
		t.Fatalf("reclaimed attempts = %#v", attempts)
	}
}

func TestStaleOwnerCannotMutateAfterEpochLoss(t *testing.T) {
	clock := newControlledClock()
	store := openQueueTestStore(t, filepath.Join(t.TempDir(), "deliveries.db"), clock)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	if _, err := store.AdmitRunnable(context.Background(), admission); err != nil {
		t.Fatal(err)
	}
	stale, err := store.Claim(context.Background(), "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute + time.Second)
	current, err := store.Claim(context.Background(), "worker-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if current.Epoch <= stale.Epoch {
		t.Fatalf("new epoch = %d, old epoch = %d", current.Epoch, stale.Epoch)
	}
	assertStaleLease(t, store.Checkpoint(context.Background(), stale, []byte("checkpoint")))
	assertStaleLease(t, store.PublishResult(context.Background(), stale, []byte("result")))
	_, err = store.TransitionLease(context.Background(), stale, DeliveryCheckpointing)
	assertStaleLease(t, err)
	_, err = store.Finalize(context.Background(), stale, Outcome{Kind: OutcomeSucceeded})
	assertStaleLease(t, err)
	if _, err := store.Finalize(context.Background(), current, Outcome{Kind: OutcomeSucceeded}); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateAdmissionCreatesOneQueueRecord(t *testing.T) {
	clock := newControlledClock()
	store := openQueueTestStore(t, filepath.Join(t.TempDir(), "deliveries.db"), clock)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	first, err := store.AdmitRunnable(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AdmitRunnable(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != second.Version || second.State != DeliveryQueued {
		t.Fatalf("duplicate admission changed delivery: first=%#v second=%#v", first, second)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM delivery_queue`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("queue records = %d, want 1", count)
	}
}

func TestRetryScheduleAndWaitingSignalPersist(t *testing.T) {
	clock := newControlledClock()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store := openQueueTestStore(t, path, clock)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	if _, err := store.AdmitRunnable(context.Background(), admission); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(context.Background(), "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	retryAt := clock.Now().Add(10 * time.Minute)
	if _, err := store.Finalize(context.Background(), lease, Outcome{Kind: OutcomeRetry, RetryAt: retryAt, Err: errors.New("temporary")}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openQueueTestStore(t, path, clock)
	if _, err := store.Claim(context.Background(), "worker-2", time.Minute); !errors.Is(err, ErrNoRunnableDelivery) {
		t.Fatalf("claim before retry = %v", err)
	}
	var availableAt, signal string
	if err := store.db.QueryRow(`SELECT available_at, waiting_signal FROM delivery_queue WHERE delivery_id = ?`, admission.DeliveryID).Scan(&availableAt, &signal); err != nil {
		t.Fatal(err)
	}
	if availableAt != timestamp(retryAt) || signal != "retry" {
		t.Fatalf("persisted retry = (%q, %q), want (%q, retry)", availableAt, signal, timestamp(retryAt))
	}
	clock.Advance(10 * time.Minute)
	if _, err := store.Claim(context.Background(), "worker-2", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatReleaseAndWaitingSignal(t *testing.T) {
	clock := newControlledClock()
	store := openQueueTestStore(t, filepath.Join(t.TempDir(), "deliveries.db"), clock)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	if _, err := store.AdmitRunnable(context.Background(), admission); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(context.Background(), "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * time.Second)
	renewed, err := store.Heartbeat(context.Background(), lease, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.Expires.Equal(clock.Now().Add(time.Minute)) || !renewed.Heartbeat.Equal(clock.Now()) {
		t.Fatalf("renewed lease = %#v", renewed)
	}
	if err := store.Release(context.Background(), renewed); err != nil {
		t.Fatal(err)
	}
	lease, err = store.Claim(context.Background(), "worker-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finalize(context.Background(), lease, Outcome{Kind: OutcomeWaiting, WaitState: DeliveryWaitingDecision, Signal: "decision:42"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(context.Background(), "worker-3", time.Minute); !errors.Is(err, ErrNoRunnableDelivery) {
		t.Fatalf("claim waiting delivery = %v", err)
	}
	var signal string
	if err := store.db.QueryRow(`SELECT waiting_signal FROM delivery_queue WHERE delivery_id = ?`, admission.DeliveryID).Scan(&signal); err != nil {
		t.Fatal(err)
	}
	if signal != "decision:42" {
		t.Fatalf("waiting signal = %q", signal)
	}
	if err := store.SignalWaiting(context.Background(), admission.Scope, admission.DeliveryID, "wrong"); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("mismatched signal error = %v", err)
	}
	if err := store.SignalWaiting(context.Background(), admission.Scope, admission.DeliveryID, "decision:42"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(context.Background(), "worker-3", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitCancelPreventsNewWorkAndFencesOwner(t *testing.T) {
	clock := newControlledClock()
	store := openQueueTestStore(t, filepath.Join(t.TempDir(), "deliveries.db"), clock)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	if _, err := store.AdmitRunnable(context.Background(), admission); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(context.Background(), "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Cancel(context.Background(), admission.Scope, admission.DeliveryID); err != nil {
		t.Fatal(err)
	}
	assertStaleLease(t, store.Checkpoint(context.Background(), lease, []byte("late")))
	if _, err := store.Claim(context.Background(), "worker-2", time.Minute); !errors.Is(err, ErrNoRunnableDelivery) {
		t.Fatalf("claim cancelled delivery = %v", err)
	}
	delivery, err := store.Load(context.Background(), admission.Scope, admission.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.State != DeliveryCancelRequested {
		t.Fatalf("cancelled delivery state = %s", delivery.State)
	}
}

func TestCallerDisconnectDoesNotCancelDetachedWork(t *testing.T) {
	clock := newControlledClock()
	store := openQueueTestStore(t, filepath.Join(t.TempDir(), "deliveries.db"), clock)
	clientCtx, disconnect := context.WithCancel(context.Background())
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	if _, err := store.AdmitRunnable(clientCtx, admission); err != nil {
		t.Fatal(err)
	}
	disconnect()
	executed := false
	worker := newTestWorker(t, store, "worker-1", executorFunc(func(ctx context.Context, _ *Execution) Outcome {
		executed = true
		if ctx.Err() != nil {
			t.Fatalf("worker inherited client cancellation: %v", ctx.Err())
		}
		return Outcome{Kind: OutcomeSucceeded}
	}))
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !executed {
		t.Fatal("detached delivery was not executed")
	}
}

func TestWorkerServiceCanStopAndRestart(t *testing.T) {
	clock := newControlledClock()
	store := openQueueTestStore(t, filepath.Join(t.TempDir(), "deliveries.db"), clock)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	if _, err := store.AdmitRunnable(context.Background(), admission); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	completed := make(chan struct{})
	var calls int
	var callsMu sync.Mutex
	executor := executorFunc(func(ctx context.Context, _ *Execution) Outcome {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call == 1 {
			close(started)
			<-ctx.Done()
			return Outcome{Kind: OutcomeFailed, Err: ctx.Err()}
		}
		close(completed)
		return Outcome{Kind: OutcomeSucceeded}
	})
	worker := newTestWorker(t, store, "restartable-worker", executor)
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start execution")
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	if err := worker.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute + time.Second)
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("restarted worker did not reclaim execution")
	}
	deadline := time.Now().Add(time.Second)
	for {
		delivery, err := store.Load(context.Background(), admission.Scope, admission.DeliveryID)
		if err != nil {
			t.Fatal(err)
		}
		if delivery.State == DeliverySucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restarted delivery state = %s", delivery.State)
		}
		time.Sleep(time.Millisecond)
	}
	if err := worker.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryMigrationUpgradesV2QueueSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deliveries.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE delivery_schema_migrations (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL); ` + deliverySchemaV1 + `; ` + deliverySchemaV2); err != nil {
		t.Fatal(err)
	}
	for _, migration := range deliveryMigrations[:2] {
		if _, err := db.Exec(`INSERT INTO delivery_schema_migrations (version, checksum) VALUES (?, ?)`, migration.version, migration.checksum); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO delivery_deliveries (tenant_id, repository_id, delivery_id, admission_key, admission_fingerprint, state, version, current_goal_revision, policy_reference, created_at, updated_at) VALUES ('tenant', 'repo', 'legacy', 'legacy-key', 'fingerprint', 'queued', 2, 1, 'policy', '2026-09-24T12:00:00Z', '2026-09-24T12:01:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store := openQueueTestStore(t, path, newControlledClock())
	var tables int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('delivery_queue', 'delivery_worker_attempts')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 2 {
		t.Fatalf("v3 queue tables = %d, want 2", tables)
	}
	var status string
	if err := store.db.QueryRow(`SELECT status FROM delivery_queue WHERE delivery_id = 'legacy'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "ready" {
		t.Fatalf("migrated queue status = %q, want ready", status)
	}
}

func newControlledClock() *controlledClock {
	return &controlledClock{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
}

func openQueueTestStore(t *testing.T, path string, clock Clock) *DeliveryStore {
	t.Helper()
	store, err := OpenDeliveryStoreWithClock(context.Background(), path, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newTestWorker(t *testing.T, store *DeliveryStore, owner string, executor Executor) *Worker {
	t.Helper()
	worker, err := NewWorker(store, executor, WorkerConfig{OwnerID: owner, Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: 30 * time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func assertStaleLease(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrStaleLease) {
		t.Fatalf("lease error = %v, want %v", err, ErrStaleLease)
	}
}
