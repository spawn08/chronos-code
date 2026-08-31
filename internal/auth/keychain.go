// Package auth implements enterprise authentication for chronos-code: BYO
// API key storage, OS-keychain-backed credential storage, and generic
// OAuth2 Authorization-Code-with-PKCE and Device Authorization Grant
// (RFC 8628) flows with auto-refresh. It exposes plain functions intended
// to be called directly by a CLI layer implementing login/logout/auth
// status/auth refresh commands.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/zalando/go-keyring"
)

// ErrNotFound is returned by Store.Load (and surfaced by GetStatus as "not
// authenticated") when no credential is stored for a provider. Callers use
// this sentinel to distinguish "not logged in" from a genuine backend
// error.
var ErrNotFound = errors.New("auth: credential not found")

// keyringService is the go-keyring "service" name under which all
// chronos-code credentials are namespaced, one "user" per provider name.
const keyringService = "chronos-code"

// keyringBackend is the seam that makes Store unit-testable without a real
// OS keychain. realKeyringBackend implements this against the host OS
// keychain; tests inject an in-memory fake via NewStoreWithBackend.
type keyringBackend interface {
	Set(service, user, pass string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

// realKeyringBackend is the OS-keychain-backed implementation of
// keyringBackend (macOS Keychain, Linux libsecret via dbus, Windows
// Credential Manager), backed by github.com/zalando/go-keyring.
type realKeyringBackend struct{}

func (realKeyringBackend) Set(service, user, pass string) error {
	return keyring.Set(service, user, pass)
}

func (realKeyringBackend) Get(service, user string) (string, error) {
	pass, err := keyring.Get(service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return pass, err
}

func (realKeyringBackend) Delete(service, user string) error {
	err := keyring.Delete(service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// Store persists Credential values in an OS keychain, keyed by provider
// name. Because go-keyring (and OS keychains generally) has no
// cross-backend "list all entries for a service" API, Store also maintains
// a small local side index (a JSON array of provider name strings, never
// secrets) at indexPath so that List can enumerate known providers.
type Store struct {
	backend   keyringBackend
	indexPath string
}

// NewStore builds a Store backed by the real OS keychain, with its provider
// index file at ~/.chronos-code/auth/providers.json. If the user's home
// directory cannot be determined, it falls back to "." rather than
// panicking.
func NewStore() *Store {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return NewStoreWithBackend(realKeyringBackend{}, filepath.Join(home, ".chronos-code", "auth", "providers.json"))
}

// NewStoreWithBackend builds a Store with an explicit keyringBackend and
// index file path. This is exported specifically so tests can inject an
// in-memory fake backend and a temporary indexPath.
func NewStoreWithBackend(b keyringBackend, indexPath string) *Store {
	return &Store{backend: b, indexPath: indexPath}
}

// Save stores cred under provider in the keychain backend, then updates the
// local provider-name index so List can find it later.
func (s *Store) Save(provider string, cred Credential) error {
	data, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("auth: marshal credential for %q: %w", provider, err)
	}
	if err := s.backend.Set(keyringService, provider, string(data)); err != nil {
		return fmt.Errorf("auth: save credential for %q: %w", provider, err)
	}
	if err := s.addToIndex(provider); err != nil {
		return fmt.Errorf("auth: update provider index after saving %q: %w", provider, err)
	}
	return nil
}

// Load reads the credential stored for provider. If no credential is
// stored, it returns (nil, ErrNotFound).
func (s *Store) Load(provider string) (*Credential, error) {
	data, err := s.backend.Get(keyringService, provider)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("auth: load credential for %q: %w", provider, err)
	}
	var cred Credential
	if err := json.Unmarshal([]byte(data), &cred); err != nil {
		return nil, fmt.Errorf("auth: unmarshal credential for %q: %w", provider, err)
	}
	return &cred, nil
}

// Delete removes the credential stored for provider. Deleting a provider
// with no stored credential is treated as a successful no-op (idempotent
// logout). Removing the provider from the local index is best-effort: it
// does not cause Delete to fail, but a real backend deletion error is never
// silently swallowed.
func (s *Store) Delete(provider string) error {
	if err := s.backend.Delete(keyringService, provider); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("auth: delete credential for %q: %w", provider, err)
	}
	_ = s.removeFromIndex(provider) // best-effort; index staleness isn't fatal.
	return nil
}

// List returns the provider names known to the local index. If the index
// file does not exist yet, it returns an empty (not nil-erroring) slice.
func (s *Store) List() ([]string, error) {
	names, err := s.readIndex()
	if err != nil {
		return nil, err
	}
	return names, nil
}

// readIndex reads the provider-name index file, returning an empty slice
// (not an error) if the file does not exist.
func (s *Store) readIndex() ([]string, error) {
	data, err := os.ReadFile(s.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("auth: read provider index %q: %w", s.indexPath, err)
	}
	if len(data) == 0 {
		return []string{}, nil
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, fmt.Errorf("auth: parse provider index %q: %w", s.indexPath, err)
	}
	return names, nil
}

// writeIndex writes the provider-name index file, creating its parent
// directory if needed, with restrictive (0o600) permissions. This file
// lists provider *names* only, never secrets, but is kept tight anyway.
func (s *Store) writeIndex(names []string) error {
	if err := os.MkdirAll(filepath.Dir(s.indexPath), 0o700); err != nil {
		return fmt.Errorf("auth: create provider index dir: %w", err)
	}
	data, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("auth: marshal provider index: %w", err)
	}
	if err := os.WriteFile(s.indexPath, data, 0o600); err != nil {
		return fmt.Errorf("auth: write provider index %q: %w", s.indexPath, err)
	}
	return nil
}

// addToIndex adds provider to the index, read-modify-write, deduplicated.
func (s *Store) addToIndex(provider string) error {
	names, err := s.readIndex()
	if err != nil {
		return err
	}
	for _, n := range names {
		if n == provider {
			return nil // already present.
		}
	}
	names = append(names, provider)
	sort.Strings(names)
	return s.writeIndex(names)
}

// removeFromIndex removes provider from the index, read-modify-write.
func (s *Store) removeFromIndex(provider string) error {
	names, err := s.readIndex()
	if err != nil {
		return err
	}
	out := names[:0]
	for _, n := range names {
		if n != provider {
			out = append(out, n)
		}
	}
	return s.writeIndex(out)
}
