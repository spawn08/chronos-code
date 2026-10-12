package cli

import "testing"

func TestParseCleanupFlags(t *testing.T) {
	dryRun, scope, err := parseCleanupFlags([]string{"--dry-run", "--scope", "events"})
	if err != nil {
		t.Fatal(err)
	}
	if !dryRun || scope != "events" {
		t.Fatalf("flags = %v %q", dryRun, scope)
	}
}

func TestParseCleanupFlagsRejectsDuplicates(t *testing.T) {
	if _, _, err := parseCleanupFlags([]string{"--dry-run", "--dry-run"}); err == nil {
		t.Fatal("duplicate --dry-run accepted")
	}
}

func TestParseCleanupFlagsRejectsRemovedPlanScopeFlags(t *testing.T) {
	if _, _, err := parseCleanupFlags([]string{"--tenant", "t"}); err == nil {
		t.Fatal("removed --tenant flag accepted")
	}
}
