package execution

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestDeliveryIntegrationFenceRequiresLiveEpochAndExtendsOwnership(t *testing.T) {
	ctx := context.Background()
	clock := newControlledClock()
	store := openQueueTestStore(t, filepath.Join(t.TempDir(), "deliveries.db"), clock)
	admission := testAdmission("tenant", "repo", "delivery", "request")
	if _, err := store.AdmitRunnable(ctx, admission); err != nil {
		t.Fatal(err)
	}
	old, err := store.Claim(ctx, "old", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute + time.Second)
	invoked := false
	if err := store.WithLeaseEffect(ctx, old, 2*time.Minute, func(context.Context) error { invoked = true; return nil }); !errors.Is(err, ErrStaleLease) || invoked {
		t.Fatalf("expired owner entered integration: called=%v error=%v", invoked, err)
	}
	current, err := store.Claim(ctx, "replacement", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithLeaseEffect(ctx, old, 2*time.Minute, func(context.Context) error { invoked = true; return nil }); !errors.Is(err, ErrStaleLease) || invoked {
		t.Fatalf("reclaimed owner entered integration: called=%v error=%v", invoked, err)
	}
	if err := store.WithLeaseEffect(ctx, current, 2*time.Minute, func(context.Context) error { invoked = true; return nil }); err != nil || !invoked {
		t.Fatalf("current owner integration: called=%v error=%v", invoked, err)
	}
	clock.Advance(time.Minute + time.Second)
	if _, err := store.Heartbeat(ctx, current, time.Minute); err != nil {
		t.Fatalf("integration fence did not preserve live ownership: %v", err)
	}
	if _, err := store.Finalize(ctx, old, Outcome{Kind: OutcomeSucceeded}); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("late stale owner finalized: %v", err)
	}
}

func TestDeliveryClaimCannotStealLeaseDuringFencedIntegration(t *testing.T) {
	ctx := context.Background()
	clock := newControlledClock()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store := openQueueTestStore(t, path, clock)
	other := openQueueTestStore(t, path, clock)
	if _, err := store.AdmitRunnable(ctx, testAdmission("tenant", "repo", "delivery", "key")); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "current", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * time.Second)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.WithLeaseEffect(ctx, lease, 2*time.Minute, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("integration did not acquire the delivery fence")
	}
	clock.Advance(40 * time.Second) // original lease expired, fenced renewal has not
	claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	claimDone := make(chan error, 1)
	go func() {
		_, err := other.Claim(claimCtx, "replacement", time.Minute)
		claimDone <- err
	}()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("fenced integration: %v", err)
	}
	if err := <-claimDone; err == nil {
		t.Fatal("replacement claimed an integration still owned by the live lease")
	}
	if _, err := other.Claim(ctx, "replacement", time.Minute); !errors.Is(err, ErrNoRunnableDelivery) {
		t.Fatalf("replacement claimed renewed lease after integration: %v", err)
	}
}
