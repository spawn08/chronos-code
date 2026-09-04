package plan

import "errors"

var (
	ErrMissingTrustedTenant = errors.New("missing trusted plan tenant")
	ErrTenantScopeMismatch  = errors.New("plan tenant scope mismatch")
)

// LocalTenantID is the explicit tenant used by local single-user operation.
const LocalTenantID TenantID = "local"

// AdmitPlanScope derives an admitted scope from the trusted tenant identity.
// Callers must provide the same tenant in their requested scope.
func AdmitPlanScope(trusted TenantID, requested PlanScope) (PlanScope, error) {
	if trusted == "" {
		return PlanScope{}, ErrMissingTrustedTenant
	}
	if requested.TenantID == "" || requested.TenantID != trusted {
		return PlanScope{}, ErrTenantScopeMismatch
	}
	if err := requested.Validate(); err != nil {
		return PlanScope{}, err
	}
	return PlanScope{TenantID: trusted, RepositoryID: requested.RepositoryID}, nil
}

// LocalPlanScope creates the explicit local-mode scope for a repository.
func LocalPlanScope(repositoryID RepositoryID) (PlanScope, error) {
	return AdmitPlanScope(LocalTenantID, PlanScope{TenantID: LocalTenantID, RepositoryID: repositoryID})
}
