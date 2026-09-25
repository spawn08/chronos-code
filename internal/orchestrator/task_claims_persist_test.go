package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/claims"
	"github.com/spawn08/chronos-code/internal/execution"
)

func addReadClaim(t *testing.T, runtime *taskRuntime) claims.Claim {
	t.Helper()
	c, err := runtime.claims.Add(claims.Input{
		Text:    "read p.go:3-5",
		Anchors: []claims.Anchor{{Path: "p.go", StartLine: 3, EndLine: 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.saveClaims(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPersistedClaimsCarryAcrossReopenAndGoStale(t *testing.T) {
	o, root := ledgerOrch(t, true)
	if err := os.WriteFile(filepath.Join(root, "p.go"), []byte(claimsSource), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := o.openTaskRuntime("plan-node-1", true, root)
	if err != nil {
		t.Fatal(err)
	}
	claim := addReadClaim(t, first)

	second, err := o.openTaskRuntime("plan-node-1", true, root)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := second.claims.Get(claim.ID)
	if !ok || got.Status != claims.StatusLive {
		t.Fatalf("claim after reopen = %+v, %v; want live", got, ok)
	}
	message := second.withWorkingMemory(context.Background(), "do the task")
	if !strings.HasPrefix(message, "do the task\n\n") || !strings.Contains(message, "read p.go:3-5") {
		t.Fatalf("working memory not appended to turn prompt: %q", message)
	}

	// Change the read span while no task is running.
	changed := strings.Replace(claimsSource, "return 1", "return 42", 1)
	if err := os.WriteFile(filepath.Join(root, "p.go"), []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := o.openTaskRuntime("plan-node-1", true, root)
	if err != nil {
		t.Fatal(err)
	}
	got, ok = third.claims.Get(claim.ID)
	if !ok || got.Status != claims.StatusStale {
		t.Fatalf("claim after external change = %+v, %v; want stale", got, ok)
	}
	if len(claimEvents(t, third)) == 0 {
		t.Fatal("stale transition on reopen was not recorded in the ledger")
	}
	// New claims must not reuse restored IDs.
	next, err := third.claims.Add(claims.Input{Text: "read p.go:7-9", Anchors: []claims.Anchor{{Path: "p.go", StartLine: 7, EndLine: 9}}})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == claim.ID {
		t.Fatalf("new claim reused restored id %s", next.ID)
	}
}

func TestClaimsNotPersistedWithoutOptIn(t *testing.T) {
	o, root := ledgerOrch(t, false)
	if err := os.WriteFile(filepath.Join(root, "p.go"), []byte(claimsSource), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := o.openTaskRuntime("plan-node-1", true, root)
	if err != nil {
		t.Fatal(err)
	}
	if first.claimsPath != "" {
		t.Fatalf("claimsPath = %q without ledger.persist", first.claimsPath)
	}
	addReadClaim(t, first)
	second, err := o.openTaskRuntime("plan-node-1", true, root)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(second.claims.List()); n != 0 {
		t.Fatalf("claims carried over without opt-in: %d", n)
	}
	if message := second.withWorkingMemory(context.Background(), "do the task"); message != "do the task" {
		t.Fatalf("prompt changed with no claims: %q", message)
	}
}

func TestCorruptClaimsSnapshotIsDiscardedWithAudit(t *testing.T) {
	o, root := ledgerOrch(t, true)
	if err := os.WriteFile(filepath.Join(root, "p.go"), []byte(claimsSource), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := o.openTaskRuntime("plan-node-1", true, root)
	if err != nil {
		t.Fatal(err)
	}
	addReadClaim(t, first)
	if err := os.WriteFile(first.claimsPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := o.openTaskRuntime("plan-node-1", true, root)
	if err != nil {
		t.Fatalf("corrupt claims snapshot blocked the task: %v", err)
	}
	if n := len(second.claims.List()); n != 0 {
		t.Fatalf("claims from corrupt snapshot = %d, want 0", n)
	}
	var audited bool
	for _, event := range claimEvents(t, second) {
		if event.Type == execution.EventClaim && strings.Contains(event.Detail, "claims snapshot discarded") {
			audited = true
		}
	}
	if !audited {
		t.Fatal("discarded claims snapshot was not recorded in the ledger")
	}
}
