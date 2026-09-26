package execution

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"
)

func TestAgentReplyCheckpointRequiresReconciledCallAndLiveLease(t *testing.T) {
	ctx := context.Background()
	clock := newControlledClock()
	path := filepath.Join(t.TempDir(), "delivery.db")
	store := openQueueTestStore(t, path, clock)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	if _, err := store.AdmitRunnable(ctx, admission); err != nil {
		t.Fatal(err)
	}
	old, err := store.Claim(ctx, "old", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	response := &model.ChatResponse{Content: "verified", StopReason: model.StopReasonEnd, UsageKnown: true}
	if err := store.CheckpointAgentReply(ctx, old, "invocation", "reader", "", "task", "fixture", response); !errors.Is(err, ErrUsageOutcomeUnknown) {
		t.Fatalf("unbilled reply checkpoint = %v", err)
	}
	reservation := UsageReservation{CallID: "invocation:provider:1", Provider: "fixture", Model: "fixture"}
	if _, err := store.ReserveUsageLease(ctx, old, reservation); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckpointAgentReply(ctx, old, "invocation", "reader", "", "task", "fixture", response); !errors.Is(err, ErrUsageOutcomeUnknown) {
		t.Fatalf("outstanding reply checkpoint = %v", err)
	}
	if _, err := store.ReconcileUsage(ctx, admission.Scope, admission.DeliveryID, reservation.CallID, IncurredUsage{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckpointAgentReply(ctx, old, "invocation", "reader", "", "task", "fixture", response); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute + time.Second)
	store = openQueueTestStore(t, path, clock)
	replacement, err := store.Claim(ctx, "replacement", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := store.ResumeAgentReply(ctx, replacement, "reader", "", "task", "fixture")
	if err != nil || prior == nil || prior.Content != response.Content || !prior.UsageKnown {
		t.Fatalf("recovered zero-usage reply = %+v, error=%v", prior, err)
	}
	if count, err := store.CheckpointedCallCount(ctx, replacement); err != nil || count != 1 {
		t.Fatalf("covered calls = %d, error=%v", count, err)
	}
	if _, err := store.ResumeAgentReply(ctx, replacement, "reader", "", "different", "fixture"); !errors.Is(err, ErrEffectNeedsReconciliation) {
		t.Fatalf("changed input resumed reply: %v", err)
	}
	if _, err := store.ResumeAgentReply(ctx, replacement, "reader", "", "task", "other-model"); !errors.Is(err, ErrEffectNeedsReconciliation) {
		t.Fatalf("changed model resumed reply: %v", err)
	}
	if err := store.CheckpointAgentReply(ctx, old, "stale", "reader", "", "task", "fixture", response); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("expired worker checkpointed reply: %v", err)
	}
}

func TestAgentReplyCheckpointFailureCannotBecomeReplayReceipt(t *testing.T) {
	ctx := context.Background()
	clock := newControlledClock()
	path := filepath.Join(t.TempDir(), "delivery.db")
	store := openQueueTestStore(t, path, clock)
	admission := testAdmission("tenant", "repo", "delivery", "admission")
	if _, err := store.AdmitRunnable(ctx, admission); err != nil {
		t.Fatal(err)
	}
	old, err := store.Claim(ctx, "old", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	callID := "invocation:provider:1"
	if _, err := store.ReserveUsageLease(ctx, old, UsageReservation{CallID: callID, Provider: "fixture", Model: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileUsage(ctx, admission.Scope, admission.DeliveryID, callID, IncurredUsage{InputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_agent_reply BEFORE INSERT ON delivery_agent_replies BEGIN SELECT RAISE(ABORT, 'injected crash before reply receipt'); END`); err != nil {
		t.Fatal(err)
	}
	response := &model.ChatResponse{Content: "uncheckpointed", StopReason: model.StopReasonEnd, UsageKnown: true}
	if err := store.CheckpointAgentReply(ctx, old, "invocation", "reader", "", "task", "fixture", response); err == nil {
		t.Fatal("lost receipt persistence was ignored")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute + time.Second)
	store = openQueueTestStore(t, path, clock)
	replacement, err := store.Claim(ctx, "replacement", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := store.CheckpointedCallCount(ctx, replacement); err != nil || count != 0 {
		t.Fatalf("missing receipt covered %d calls, err=%v", count, err)
	}
	if prior, err := store.ResumeAgentReply(ctx, replacement, "reader", "", "task", "fixture"); err != nil || prior != nil {
		t.Fatalf("uncheckpointed reply replayed: %+v, err=%v", prior, err)
	}
	if usage, err := store.Usage(ctx, admission.Scope, admission.DeliveryID); err != nil || usage.ReconciledCalls != 1 {
		t.Fatalf("original billing was lost: %+v, err=%v", usage, err)
	}
}
