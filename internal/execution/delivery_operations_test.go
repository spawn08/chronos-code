package execution

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestOperationJournalNeverReplaysAnAmbiguousEffect(t *testing.T) {
	ctx := context.Background()
	clock := newControlledClock()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store := openQueueTestStore(t, path, clock)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	if _, err := store.AdmitRunnable(ctx, admission); err != nil {
		t.Fatal(err)
	}
	first, err := store.Claim(ctx, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := Operation{ID: "call-1", EffectKey: "effect-1", Kind: "shell", ReplayClass: ReplayUnknown, InputFingerprint: "input-sha256"}
	prepared, err := store.PrepareOperation(ctx, first, request)
	if err != nil || prepared.Status != OperationPrepared {
		t.Fatalf("prepare = %+v, error = %v", prepared, err)
	}
	if _, err := store.PrepareOperation(ctx, first, request); err != nil {
		t.Fatalf("prepare retry before effect: %v", err)
	}
	otherID := request
	otherID.ID = "call-2"
	if _, err := store.PrepareOperation(ctx, first, otherID); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("reused effect key = %v", err)
	}
	conflict := request
	conflict.InputFingerprint = "changed"
	if _, err := store.PrepareOperation(ctx, first, conflict); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("changed operation = %v", err)
	}
	if _, err := store.BeginOperation(ctx, first, request.ID); err != nil {
		t.Fatal(err)
	}
	// An effect may have happened after Begin; simulating a crash and a new
	// lease must not silently authorize the same call a second time.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute + time.Second)
	store = openQueueTestStore(t, path, clock)
	replacement, err := store.Claim(ctx, "worker-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareOperation(ctx, replacement, request); !errors.Is(err, ErrEffectNeedsReconciliation) {
		t.Fatalf("ambiguous effect replay = %v", err)
	}
	if _, err := store.ObserveOperation(ctx, first, request.ID, json.RawMessage(`{"ok":true}`), "after", ""); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("stale owner observed an effect: %v", err)
	}
	if _, err := store.ReconcileOperation(ctx, replacement, request.ID, "", "after", nil); !errors.Is(err, ErrInvalidDelivery) {
		t.Fatalf("unproven reconciliation = %v", err)
	}
	reconciled, err := store.ReconcileOperation(ctx, replacement, request.ID, "inspected external state", "after", json.RawMessage(`{"ok":true}`))
	if err != nil || reconciled.Status != OperationReconciled || reconciled.OutputFingerprint != "after" {
		t.Fatalf("reconciliation = %+v, error = %v", reconciled, err)
	}
	if _, err := store.PrepareOperation(ctx, replacement, request); err != nil {
		t.Fatalf("reconciled operation lookup: %v", err)
	}
	if _, err := store.BeginOperation(ctx, replacement, request.ID); !errors.Is(err, ErrInvalidDeliveryTransition) {
		t.Fatalf("reconciled operation started again: %v", err)
	}
	if _, err := store.Operation(ctx, DeliveryScope{TenantID: "other", RepositoryID: "repo"}, admission.DeliveryID, request.ID); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("cross-tenant operation lookup = %v", err)
	}
	events, err := store.Events(ctx, admission.Scope, admission.DeliveryID)
	if err != nil || len(events) != 6 || events[3].Type != DeliveryEventOperationPrepared || events[4].Type != DeliveryEventOperationRunning || events[5].Type != DeliveryEventOperationReconciled {
		t.Fatalf("operation event lineage = %+v, error = %v", events, err)
	}
}

func TestOperationJournalObserveIsAtomicWithResultEvent(t *testing.T) {
	ctx := context.Background()
	store := openQueueTestStore(t, filepath.Join(t.TempDir(), "deliveries.db"), newControlledClock())
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	if _, err := store.AdmitRunnable(ctx, admission); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{ID: "write-1", EffectKey: "file:main.go", Kind: "file_write", ReplayClass: ReplayFingerprintedWrite, InputFingerprint: "before"}
	if _, err := store.PrepareOperation(ctx, lease, op); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginOperation(ctx, lease, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_observation BEFORE UPDATE OF status ON delivery_operations WHEN NEW.status = 'observed' BEGIN SELECT RAISE(ABORT, 'lost observation'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ObserveOperation(ctx, lease, op.ID, json.RawMessage(`{"written":true}`), "after", ""); err == nil {
		t.Fatal("observation storage failure was ignored")
	}
	loaded, err := store.Operation(ctx, admission.Scope, admission.DeliveryID, op.ID)
	if err != nil || loaded.Status != OperationRunning || loaded.OutputFingerprint != "" {
		t.Fatalf("partial observation = %+v, error = %v", loaded, err)
	}
	if _, err := store.PrepareOperation(ctx, lease, op); !errors.Is(err, ErrEffectNeedsReconciliation) {
		t.Fatalf("running operation replay = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_observation`); err != nil {
		t.Fatal(err)
	}
	observed, err := store.ObserveOperation(ctx, lease, op.ID, json.RawMessage(`{"written":true}`), "after", "")
	if err != nil || observed.Status != OperationObserved {
		t.Fatalf("observed operation = %+v, error = %v", observed, err)
	}
}

func TestPreparedOperationCanResumeAfterWorkerDiesBeforeEffect(t *testing.T) {
	ctx := context.Background()
	clock := newControlledClock()
	store := openQueueTestStore(t, filepath.Join(t.TempDir(), "deliveries.db"), clock)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	if _, err := store.AdmitRunnable(ctx, admission); err != nil {
		t.Fatal(err)
	}
	old, err := store.Claim(ctx, "old", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{ID: "call", EffectKey: "effect", Kind: "file_write", ReplayClass: ReplayFingerprintedWrite, InputFingerprint: "before"}
	if _, err := store.PrepareOperation(ctx, old, op); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute + time.Second)
	current, err := store.Claim(ctx, "replacement", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := store.PrepareOperation(ctx, current, op)
	if err != nil || resumed.Status != OperationPrepared {
		t.Fatalf("resume prepare = %+v, error = %v", resumed, err)
	}
	if _, err := store.BeginOperation(ctx, old, op.ID); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("stale worker began effect: %v", err)
	}
	started, err := store.BeginOperation(ctx, current, op.ID)
	if err != nil || started.LeaseEpoch != current.Epoch {
		t.Fatalf("replacement began effect = %+v, error = %v", started, err)
	}
	if _, err := store.ObserveOperation(ctx, current, op.ID, json.RawMessage(`{"written":true}`), "after", ""); err != nil {
		t.Fatal(err)
	}
}
