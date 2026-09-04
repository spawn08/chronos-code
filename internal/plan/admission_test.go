package plan

import (
	"errors"
	"testing"
)

func TestTenantAdmission(t *testing.T) {
	t.Run("rejects missing trusted tenant", func(t *testing.T) {
		_, err := AdmitPlanScope("", PlanScope{TenantID: "tenant-a", RepositoryID: "repo"})
		if !errors.Is(err, ErrMissingTrustedTenant) {
			t.Fatalf("AdmitPlanScope() error = %v, want %v", err, ErrMissingTrustedTenant)
		}
	})

	t.Run("rejects empty requested tenant", func(t *testing.T) {
		_, err := AdmitPlanScope("tenant-a", PlanScope{RepositoryID: "repo"})
		if !errors.Is(err, ErrTenantScopeMismatch) {
			t.Fatalf("AdmitPlanScope() error = %v, want %v", err, ErrTenantScopeMismatch)
		}
	})

	t.Run("rejects mismatched requested tenant", func(t *testing.T) {
		_, err := AdmitPlanScope("tenant-a", PlanScope{TenantID: "tenant-b", RepositoryID: "repo"})
		if !errors.Is(err, ErrTenantScopeMismatch) {
			t.Fatalf("AdmitPlanScope() error = %v, want %v", err, ErrTenantScopeMismatch)
		}
	})

	t.Run("derives scope from trusted tenant", func(t *testing.T) {
		scope, err := AdmitPlanScope("tenant-a", PlanScope{TenantID: "tenant-a", RepositoryID: "repo"})
		if err != nil {
			t.Fatalf("AdmitPlanScope() error = %v", err)
		}
		want := PlanScope{TenantID: "tenant-a", RepositoryID: "repo"}
		if scope != want {
			t.Fatalf("AdmitPlanScope() = %#v, want %#v", scope, want)
		}
	})

	t.Run("keeps local tenant explicit", func(t *testing.T) {
		scope, err := LocalPlanScope("repo")
		if err != nil {
			t.Fatalf("LocalPlanScope() error = %v", err)
		}
		want := PlanScope{TenantID: LocalTenantID, RepositoryID: "repo"}
		if scope != want {
			t.Fatalf("LocalPlanScope() = %#v, want %#v", scope, want)
		}
	})
}
