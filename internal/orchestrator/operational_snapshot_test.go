package orchestrator

import (
	"testing"

	"github.com/spawn08/chronos-code/internal/verification"
)

func TestCloneExecutionSnapshotIsImmutable(t *testing.T) {
	original := ExecutionSnapshot{Verification: verification.Decision{Obligations: []verification.Obligation{{ID: "test", Paths: []string{"a.go"}}}}}
	cloned := cloneExecutionSnapshot(original)
	cloned.Verification.Obligations[0].ID = "changed"
	cloned.Verification.Obligations[0].Paths[0] = "changed.go"
	if original.Verification.Obligations[0].ID != "test" || original.Verification.Obligations[0].Paths[0] != "a.go" {
		t.Fatalf("snapshot clone mutated source: %#v", original)
	}
}

func TestOperationalBounds(t *testing.T) {
	strings := make([]string, maxOperationalItems+3)
	specialists := make([]SpecialistSnapshot, maxOperationalItems+3)
	if got := len(boundStrings(strings)); got != maxOperationalItems {
		t.Fatalf("boundStrings length = %d, want %d", got, maxOperationalItems)
	}
	if got := len(boundSpecialists(specialists)); got != maxOperationalItems {
		t.Fatalf("boundSpecialists length = %d, want %d", got, maxOperationalItems)
	}
}
