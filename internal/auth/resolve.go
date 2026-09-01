package auth

import (
	"context"
	"os"
	"time"
)

// CredentialSource identifies which link of a provider's precedence chain
// (ROADMAP.md §5.3) produced the effective token Resolve returned.
type CredentialSource string

const (
	SourceGatewayEnv    CredentialSource = "env:gateway_token"  // e.g. ANTHROPIC_AUTH_TOKEN, CODEX_ACCESS_TOKEN
	SourceAPIKeyEnv     CredentialSource = "env:api_key"        // e.g. ANTHROPIC_API_KEY, OPENAI_API_KEY
	SourceLongLivedEnv  CredentialSource = "env:oauth_token"    // e.g. CLAUDE_CODE_OAUTH_TOKEN
	SourceOwnOAuth      CredentialSource = "chronos-code:oauth" // chronos-code's own `auth login --oauth-pkce`/`--device-code`
	SourceExternalReuse CredentialSource = "reuse:external_cli" // ~/.claude/.credentials.json or ~/.codex/auth.json
	SourceOwnAPIKey     CredentialSource = "chronos-code:api_key"
	SourceNone          CredentialSource = "none"
)

// ResolvedCredential is the outcome of running a provider's precedence
// chain: the token to place in ModelConfig.APIKey, plus enough provenance
// for `auth whoami`/`auth providers` to explain where it came from.
type ResolvedCredential struct {
	Provider  string
	Token     string
	Source    CredentialSource
	Method    Method
	ExpiresAt time.Time
}

// Resolve dispatches to the provider-specific precedence chain for
// "anthropic"/"claude" and "openai"/"codex", auto-refreshing chronos-code's
// own stored OAuth credential (within DefaultRefreshWindow of expiry) along
// the way. Any other provider name falls back to chronos-code's own stored
// API-key credential only — the same behavior this package had before the
// chains below existed, so unrelated providers (gemini, mistral, ...) are
// unaffected.
func Resolve(ctx context.Context, store *Store, provider string) ResolvedCredential {
	switch provider {
	case "anthropic", "claude":
		return resolveAnthropic(ctx, store)
	case "openai", "codex":
		return resolveOpenAI(ctx, store)
	default:
		return resolveGeneric(store, provider)
	}
}

// resolveAnthropic implements: ANTHROPIC_AUTH_TOKEN > ANTHROPIC_API_KEY >
// CLAUDE_CODE_OAUTH_TOKEN > chronos-code's own stored OAuth credential >
// ~/.claude/.credentials.json reuse > chronos-code's own stored API-key
// credential.
func resolveAnthropic(ctx context.Context, store *Store) ResolvedCredential {
	const provider = "anthropic"
	if v := os.Getenv("ANTHROPIC_AUTH_TOKEN"); v != "" {
		return ResolvedCredential{Provider: provider, Token: v, Source: SourceGatewayEnv, Method: MethodAPIKey}
	}
	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		return ResolvedCredential{Provider: provider, Token: v, Source: SourceAPIKeyEnv, Method: MethodAPIKey}
	}
	if v := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); v != "" {
		return ResolvedCredential{Provider: provider, Token: v, Source: SourceLongLivedEnv, Method: MethodOAuthPKCE}
	}
	if rc, ok := ownOAuthCredential(ctx, store, provider); ok {
		return rc
	}
	if cred, err := LoadClaudeCodeCredential(); err == nil {
		return ResolvedCredential{Provider: provider, Token: cred.AccessToken, Source: SourceExternalReuse, Method: cred.Method, ExpiresAt: cred.ExpiresAt}
	}
	return resolveGeneric(store, provider)
}

// resolveOpenAI implements: CODEX_ACCESS_TOKEN > OPENAI_API_KEY >
// chronos-code's own stored OAuth credential > ~/.codex/auth.json reuse >
// chronos-code's own stored API-key credential. (Device-code login is a
// user-initiated flow, not part of passive resolution — see
// LoginDeviceCode.)
func resolveOpenAI(ctx context.Context, store *Store) ResolvedCredential {
	const provider = "openai"
	if v := os.Getenv("CODEX_ACCESS_TOKEN"); v != "" {
		return ResolvedCredential{Provider: provider, Token: v, Source: SourceGatewayEnv, Method: MethodAPIKey}
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		return ResolvedCredential{Provider: provider, Token: v, Source: SourceAPIKeyEnv, Method: MethodAPIKey}
	}
	if rc, ok := ownOAuthCredential(ctx, store, provider); ok {
		return rc
	}
	if cred, err := LoadCodexCredential(); err == nil {
		token := cred.AccessToken
		if token == "" {
			token = cred.APIKey
		}
		return ResolvedCredential{Provider: provider, Token: token, Source: SourceExternalReuse, Method: cred.Method, ExpiresAt: cred.ExpiresAt}
	}
	return resolveGeneric(store, provider)
}

// ownOAuthCredential loads chronos-code's own stored credential for
// provider, auto-refreshing it first if it's an OAuth credential nearing
// expiry. It reports ok=false for anything that isn't a usable OAuth
// credential (missing, API-key method, or empty access token), so the
// caller falls through to the next link in the chain.
func ownOAuthCredential(ctx context.Context, store *Store, provider string) (ResolvedCredential, bool) {
	_ = AutoRefreshStored(ctx, store, provider, DefaultRefreshWindow) // best-effort; fall through to a stale/absent token on failure.
	cred, err := store.Load(provider)
	if err != nil || cred.Method == MethodAPIKey || cred.AccessToken == "" {
		return ResolvedCredential{}, false
	}
	return ResolvedCredential{Provider: provider, Token: cred.AccessToken, Source: SourceOwnOAuth, Method: cred.Method, ExpiresAt: cred.ExpiresAt}, true
}

// resolveGeneric is the last link common to every chain: chronos-code's own
// stored API-key credential (`auth login <provider> --api-key ...`). It is
// also the entire chain for providers with no dedicated precedence rules.
func resolveGeneric(store *Store, provider string) ResolvedCredential {
	if cred, err := store.Load(provider); err == nil && cred.Method == MethodAPIKey && cred.APIKey != "" {
		return ResolvedCredential{Provider: provider, Token: cred.APIKey, Source: SourceOwnAPIKey, Method: MethodAPIKey}
	}
	return ResolvedCredential{Provider: provider, Source: SourceNone}
}
