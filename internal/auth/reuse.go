package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ErrNoExternalCredential is returned by the LoadXxxCredential functions
// below when the external CLI's credential file is absent, unreadable, or
// doesn't contain a usable credential. It is deliberately not a hard error:
// "Claude Code / Codex isn't installed or logged in" is an expected,
// unremarkable state for the precedence chain in resolve.go to fall through
// on, not a failure worth surfacing to the user.
var ErrNoExternalCredential = errors.New("auth: no usable external credential found")

// claudeCodeCredentialsFile is the on-disk schema Claude Code writes to
// ~/.claude/.credentials.json on Linux (macOS instead uses the OS Keychain,
// which this package cannot read on Claude Code's behalf since it uses its
// own keychain "service" name).
type claudeCodeCredentialsFile struct {
	ClaudeAiOauth struct {
		AccessToken  string   `json:"accessToken"`
		RefreshToken string   `json:"refreshToken"`
		ExpiresAt    int64    `json:"expiresAt"` // milliseconds since epoch
		Scopes       []string `json:"scopes"`
	} `json:"claudeAiOauth"`
}

// LoadClaudeCodeCredential reads Claude Code's own credential file
// (~/.claude/.credentials.json) and converts it to a Credential, for the
// "reuse an existing Claude Code login" step of the Anthropic precedence
// chain (ROADMAP.md §5.3). It returns ErrNoExternalCredential if the file is
// missing, unparseable, or has no access token — never for "Claude Code
// isn't installed," which is the common case.
func LoadClaudeCodeCredential() (*Credential, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, ErrNoExternalCredential
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return nil, ErrNoExternalCredential
	}
	var f claudeCodeCredentialsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, ErrNoExternalCredential
	}
	if f.ClaudeAiOauth.AccessToken == "" {
		return nil, ErrNoExternalCredential
	}
	return &Credential{
		Provider:     "anthropic",
		Method:       MethodOAuthPKCE,
		AccessToken:  f.ClaudeAiOauth.AccessToken,
		RefreshToken: f.ClaudeAiOauth.RefreshToken,
		ExpiresAt:    time.UnixMilli(f.ClaudeAiOauth.ExpiresAt),
		Scopes:       f.ClaudeAiOauth.Scopes,
	}, nil
}

// codexAuthFile is the on-disk schema the OpenAI Codex CLI writes to
// ~/.codex/auth.json. auth_mode is "apiKey" when OPENAI_API_KEY is the
// active credential, or "chatgpt"/"chatgptAuthTokens" when Tokens holds a
// ChatGPT OAuth session.
type codexAuthFile struct {
	AuthMode     string `json:"auth_mode"`
	OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	Tokens       struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

// LoadCodexCredential reads the OpenAI Codex CLI's credential file
// (~/.codex/auth.json) and converts it to a Credential, for the "reuse an
// existing Codex CLI login" step of the OpenAI precedence chain
// (ROADMAP.md §5.3). It prefers the ChatGPT OAuth tokens when present (Codex
// writes both in "chatgpt" mode with the API key left over from a prior
// session), otherwise falls back to the plain API key. Codex's auth.json has
// no expiry field, so ExpiresAt is left zero (never expires from this
// package's point of view; Codex itself handles token refresh internally
// when its own CLI runs, which chronos-code doesn't drive).
func LoadCodexCredential() (*Credential, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, ErrNoExternalCredential
	}
	data, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		return nil, ErrNoExternalCredential
	}
	var f codexAuthFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, ErrNoExternalCredential
	}
	if f.Tokens.AccessToken != "" {
		return &Credential{
			Provider:    "openai",
			Method:      MethodOAuthPKCE,
			AccessToken: f.Tokens.AccessToken,
		}, nil
	}
	if f.OpenAIAPIKey != "" {
		return &Credential{
			Provider: "openai",
			Method:   MethodAPIKey,
			APIKey:   f.OpenAIAPIKey,
		}, nil
	}
	return nil, ErrNoExternalCredential
}
