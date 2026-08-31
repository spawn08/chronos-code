package auth

import "time"

// Method identifies how a provider's credential was obtained.
type Method string

const (
	MethodAPIKey     Method = "api_key"
	MethodOAuthPKCE  Method = "oauth_pkce"
	MethodDeviceCode Method = "device_code"
)

// Credential is the persisted authentication state for a single provider.
type Credential struct {
	Provider     string    `json:"provider"`
	Method       Method    `json:"method"`
	APIKey       string    `json:"api_key,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"` // zero value = never expires (e.g. API keys)
}

// LoginAPIKey stores a bring-your-own API key credential for provider. API
// keys never expire (ExpiresAt is left at its zero value).
func LoginAPIKey(store *Store, provider, apiKey string) error {
	return store.Save(provider, Credential{
		Provider: provider,
		Method:   MethodAPIKey,
		APIKey:   apiKey,
	})
}

// Logout removes the stored credential for provider. It is idempotent:
// logging out a provider that was never logged in succeeds.
func Logout(store *Store, provider string) error {
	return store.Delete(provider)
}

// Status is a point-in-time snapshot of a provider's authentication state,
// suitable for display by an `auth status` command.
type Status struct {
	Provider      string
	Method        Method
	Authenticated bool
	ExpiresAt     time.Time
	// ExpiresIn is a human-readable rendering of ExpiresAt: "never" for a
	// zero ExpiresAt, "expired" if ExpiresAt is in the past, otherwise a
	// duration string such as "2h14m0s".
	ExpiresIn string
}

// GetStatus reports the authentication status for provider. A provider
// with no stored credential is not an error: it yields
// Status{Authenticated: false}.
func GetStatus(store *Store, provider string) (Status, error) {
	cred, err := store.Load(provider)
	if err != nil {
		if err == ErrNotFound {
			return Status{Provider: provider, Authenticated: false}, nil
		}
		return Status{}, err
	}
	return Status{
		Provider:      provider,
		Method:        cred.Method,
		Authenticated: true,
		ExpiresAt:     cred.ExpiresAt,
		ExpiresIn:     expiresInString(cred.ExpiresAt),
	}, nil
}

// ListStatus reports the authentication status for every provider known to
// store's index.
func ListStatus(store *Store) ([]Status, error) {
	names, err := store.List()
	if err != nil {
		return nil, err
	}
	statuses := make([]Status, 0, len(names))
	for _, name := range names {
		st, err := GetStatus(store, name)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, st)
	}
	return statuses, nil
}

// expiresInString renders an expiry time as described on Status.ExpiresIn.
func expiresInString(expiresAt time.Time) string {
	if expiresAt.IsZero() {
		return "never"
	}
	d := time.Until(expiresAt)
	if d <= 0 {
		return "expired"
	}
	return d.String()
}
