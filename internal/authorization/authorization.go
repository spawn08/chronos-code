package authorization

import (
	"context"
	"errors"
	"strings"
)

type requestKey struct{}

var ErrDenied = errors.New("authorization denied")

// Request is the complete authority tuple for a repository-scoped action.
type Request struct {
	PrincipalID  string
	TenantID     string
	RepositoryID string
	Action       string
}

// Authorizer decides whether a principal may perform one action in one tenant
// and repository. Implementations must fail closed for incomplete requests.
type Authorizer interface {
	Authorize(context.Context, Request) error
}

// WithRequest binds an already authorized request scope for downstream work.
func WithRequest(ctx context.Context, request Request) context.Context {
	return context.WithValue(ctx, requestKey{}, request)
}

// FromContext returns the authorized request scope.
func FromContext(ctx context.Context) (Request, bool) {
	request, ok := ctx.Value(requestKey{}).(Request)
	return request, ok && request.PrincipalID != "" && request.TenantID != "" && request.RepositoryID != "" && request.Action != ""
}

// RepositoryAuthorizer admits authenticated principals only to one configured
// repository. Tenant identity remains mandatory and is never request supplied.
type RepositoryAuthorizer struct {
	RepositoryID   string
	AllowedActions map[string]struct{}
}

func (a RepositoryAuthorizer) Authorize(_ context.Context, request Request) error {
	if strings.TrimSpace(request.PrincipalID) == "" || strings.TrimSpace(request.TenantID) == "" ||
		strings.TrimSpace(request.RepositoryID) == "" || strings.TrimSpace(request.Action) == "" ||
		request.RepositoryID != a.RepositoryID {
		return ErrDenied
	}
	if a.AllowedActions != nil {
		if _, ok := a.AllowedActions[request.Action]; !ok {
			return ErrDenied
		}
	}
	return nil
}
