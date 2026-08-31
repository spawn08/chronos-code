package auth

import (
	"path/filepath"
	"testing"
	"time"
)

// fakeKeyringBackend is an in-memory keyringBackend used so tests never
// touch a real OS keychain. It mirrors the "not found" contract the real
// backend must honor: Get on a missing key returns ErrNotFound.
type fakeKeyringBackend struct {
	data map[string]string // keyed by "service/user"
}

func newFakeKeyringBackend() *fakeKeyringBackend {
	return &fakeKeyringBackend{data: make(map[string]string)}
}

func (f *fakeKeyringBackend) key(service, user string) string { return service + "/" + user }

func (f *fakeKeyringBackend) Set(service, user, pass string) error {
	f.data[f.key(service, user)] = pass
	return nil
}

func (f *fakeKeyringBackend) Get(service, user string) (string, error) {
	v, ok := f.data[f.key(service, user)]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (f *fakeKeyringBackend) Delete(service, user string) error {
	k := f.key(service, user)
	if _, ok := f.data[k]; !ok {
		return ErrNotFound
	}
	delete(f.data, k)
	return nil
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStoreWithBackend(newFakeKeyringBackend(), filepath.Join(t.TempDir(), "providers.json"))
}

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	store := newTestStore(t)
	cred := Credential{
		Provider:  "acme",
		Method:    MethodAPIKey,
		APIKey:    "sk-test-123",
		ExpiresAt: time.Time{},
	}
	if err := store.Save("acme", cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load("acme")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Provider != cred.Provider || got.Method != cred.Method || got.APIKey != cred.APIKey {
		t.Fatalf("Load returned %+v, want %+v", got, cred)
	}
}

func TestStoreLoadMissingReturnsErrNotFound(t *testing.T) {
	store := newTestStore(t)
	_, err := store.Load("nope")
	if err != ErrNotFound {
		t.Fatalf("Load on missing provider = %v, want ErrNotFound", err)
	}
}

func TestStoreDeleteRoundTrip(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save("acme", Credential{Provider: "acme", Method: MethodAPIKey, APIKey: "k"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete("acme"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load("acme"); err != ErrNotFound {
		t.Fatalf("Load after Delete = %v, want ErrNotFound", err)
	}
}

func TestStoreDeleteNeverSavedIsNoop(t *testing.T) {
	store := newTestStore(t)
	if err := store.Delete("never-saved"); err != nil {
		t.Fatalf("Delete on never-saved provider should be a no-op success, got %v", err)
	}
}

func TestStoreList(t *testing.T) {
	store := newTestStore(t)

	names, err := store.List()
	if err != nil {
		t.Fatalf("List on empty store: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("List on empty store = %v, want empty", names)
	}

	if err := store.Save("beta", Credential{Provider: "beta", Method: MethodAPIKey, APIKey: "k"}); err != nil {
		t.Fatalf("Save beta: %v", err)
	}
	if err := store.Save("alpha", Credential{Provider: "alpha", Method: MethodAPIKey, APIKey: "k"}); err != nil {
		t.Fatalf("Save alpha: %v", err)
	}
	// Saving the same provider twice should not duplicate the index entry.
	if err := store.Save("alpha", Credential{Provider: "alpha", Method: MethodAPIKey, APIKey: "k2"}); err != nil {
		t.Fatalf("Save alpha again: %v", err)
	}

	names, err = store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("List = %v, want 2 entries", names)
	}

	if err := store.Delete("beta"); err != nil {
		t.Fatalf("Delete beta: %v", err)
	}
	names, err = store.List()
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(names) != 1 || names[0] != "alpha" {
		t.Fatalf("List after delete = %v, want [alpha]", names)
	}
}

func TestLoginAPIKey(t *testing.T) {
	store := newTestStore(t)
	if err := LoginAPIKey(store, "acme", "sk-abc"); err != nil {
		t.Fatalf("LoginAPIKey: %v", err)
	}
	cred, err := store.Load("acme")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cred.Method != MethodAPIKey || cred.APIKey != "sk-abc" || !cred.ExpiresAt.IsZero() {
		t.Fatalf("Load = %+v, want api key credential that never expires", cred)
	}
}

func TestGetStatusNotAuthenticated(t *testing.T) {
	store := newTestStore(t)
	st, err := GetStatus(store, "acme")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.Authenticated {
		t.Fatalf("GetStatus.Authenticated = true, want false for never-logged-in provider")
	}
	if st.Provider != "acme" {
		t.Fatalf("GetStatus.Provider = %q, want acme", st.Provider)
	}
}

func TestGetStatusAPIKeyNeverExpires(t *testing.T) {
	store := newTestStore(t)
	if err := LoginAPIKey(store, "acme", "sk-abc"); err != nil {
		t.Fatalf("LoginAPIKey: %v", err)
	}
	st, err := GetStatus(store, "acme")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !st.Authenticated || st.ExpiresIn != "never" {
		t.Fatalf("GetStatus = %+v, want authenticated with ExpiresIn=never", st)
	}
}

func TestGetStatusExpired(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save("acme", Credential{
		Provider:  "acme",
		Method:    MethodOAuthPKCE,
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := GetStatus(store, "acme")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.ExpiresIn != "expired" {
		t.Fatalf("ExpiresIn = %q, want expired", st.ExpiresIn)
	}
}

func TestListStatus(t *testing.T) {
	store := newTestStore(t)
	if err := LoginAPIKey(store, "alpha", "k1"); err != nil {
		t.Fatalf("LoginAPIKey alpha: %v", err)
	}
	if err := LoginAPIKey(store, "beta", "k2"); err != nil {
		t.Fatalf("LoginAPIKey beta: %v", err)
	}

	statuses, err := ListStatus(store)
	if err != nil {
		t.Fatalf("ListStatus: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("ListStatus = %v, want 2 entries", statuses)
	}
	for _, st := range statuses {
		if !st.Authenticated {
			t.Fatalf("ListStatus entry %+v not authenticated", st)
		}
	}
}

func TestLogout(t *testing.T) {
	store := newTestStore(t)
	if err := LoginAPIKey(store, "acme", "sk-abc"); err != nil {
		t.Fatalf("LoginAPIKey: %v", err)
	}
	if err := Logout(store, "acme"); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	st, err := GetStatus(store, "acme")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.Authenticated {
		t.Fatalf("GetStatus after Logout = %+v, want not authenticated", st)
	}
}
