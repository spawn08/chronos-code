package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spawn08/chronos-code/internal/authorization"
)

type recordingAuthorizer struct {
	request authorization.Request
	err     error
}

func (a *recordingAuthorizer) Authorize(_ context.Context, request authorization.Request) error {
	a.request = request
	return a.err
}

func TestAuthorizationMiddlewareBindsTrustedScopeAndAction(t *testing.T) {
	authorizer := &recordingAuthorizer{}
	handler := localIdentityMiddleware("tenant-a")(tenantMiddleware(authorizationMiddleware("repo-a", authorizer)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))))
	request := httptest.NewRequest(http.MethodPost, "/v1/chat", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	want := authorization.Request{PrincipalID: "local", TenantID: "tenant-a", RepositoryID: "repo-a", Action: "chat.execute"}
	if authorizer.request != want {
		t.Fatalf("authorization request = %#v, want %#v", authorizer.request, want)
	}
}

func TestAuthorizationMiddlewareFailsClosed(t *testing.T) {
	authorizer := &recordingAuthorizer{err: errors.New("denied")}
	handler := localIdentityMiddleware("tenant-a")(tenantMiddleware(authorizationMiddleware("repo-a", authorizer)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("denied request reached handler")
	}))))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/v1/sessions/id", nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}
