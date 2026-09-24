package authorization

import (
	"context"
	"errors"
	"testing"
)

func TestRepositoryAuthorizerRequiresCompleteMatchingScope(t *testing.T) {
	authorizer := RepositoryAuthorizer{RepositoryID: "repo-a", AllowedActions: map[string]struct{}{"chat.execute": {}}}
	valid := Request{PrincipalID: "user-a", TenantID: "tenant-a", RepositoryID: "repo-a", Action: "chat.execute"}
	if err := authorizer.Authorize(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	for _, request := range []Request{
		{TenantID: valid.TenantID, RepositoryID: valid.RepositoryID, Action: valid.Action},
		{PrincipalID: valid.PrincipalID, RepositoryID: valid.RepositoryID, Action: valid.Action},
		{PrincipalID: valid.PrincipalID, TenantID: valid.TenantID, RepositoryID: "repo-b", Action: valid.Action},
		{PrincipalID: valid.PrincipalID, TenantID: valid.TenantID, RepositoryID: valid.RepositoryID},
		{PrincipalID: valid.PrincipalID, TenantID: valid.TenantID, RepositoryID: valid.RepositoryID, Action: "delivery.cancel"},
	} {
		if err := authorizer.Authorize(context.Background(), request); !errors.Is(err, ErrDenied) {
			t.Fatalf("Authorize(%#v) error = %v, want denied", request, err)
		}
	}
}
