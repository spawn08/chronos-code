package cli

import "testing"

func TestParseCleanupFlags(t *testing.T) {
	dryRun, scope, tenant, repository, err := parseCleanupFlags([]string{"--dry-run", "--scope", "events", "--tenant=t", "--repository", "r"})
	if err != nil {
		t.Fatal(err)
	}
	if !dryRun || scope != "events" || tenant != "t" || repository != "r" {
		t.Fatalf("flags = %v %q %q %q", dryRun, scope, tenant, repository)
	}
}

func TestParseCleanupFlagsRejectsDuplicates(t *testing.T) {
	if _, _, _, _, err := parseCleanupFlags([]string{"--dry-run", "--dry-run"}); err == nil {
		t.Fatal("duplicate --dry-run accepted")
	}
}
