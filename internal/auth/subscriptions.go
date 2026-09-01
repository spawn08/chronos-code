package auth

// OpenAICodexSubscriptionConfig returns the ProviderOAuthConfig for
// "sign in with your ChatGPT Plus/Pro subscription" — the same OAuth
// client the official Codex CLI itself uses. Every value here is
// community-documented (reverse-engineered from Codex CLI's own traffic
// and published in several independent open-source OAuth
// implementations), not something OpenAI has published as a public API
// for third parties to use.
//
// This is deliberately implemented only for OpenAI, not Anthropic. The
// equivalent Claude Code client_id reuse — the pattern tools like
// OpenClaw and OpenCode used — was explicitly banned by Anthropic in
// February 2026 (a ToS change) and technically blocked in April 2026;
// attempting it now would violate Anthropic's current terms and simply
// fail. OpenAI, as of this writing, has not restricted third-party use of
// this client, and ChatGPT Plus/Pro subscribers reportedly authenticate
// third-party harnesses with it directly — but that could change at any
// time without notice, exactly as Anthropic's equivalent did. If OpenAI
// closes this the same way, this flow stops working; LoginPKCE simply
// surfaces the resulting HTTP error, and chronos-code's API-key and
// existing-Codex-CLI-login paths are unaffected either way.
func OpenAICodexSubscriptionConfig() ProviderOAuthConfig {
	return ProviderOAuthConfig{
		Provider:     "openai",
		ClientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
		AuthURL:      "https://auth.openai.com/oauth/authorize",
		TokenURL:     "https://auth.openai.com/oauth/token",
		Scopes:       []string{"openid", "profile", "email", "offline_access"},
		RedirectPort: 1455,
		RedirectPath: "/auth/callback",
		// These three are OpenAI-specific flags Codex CLI sends on every
		// authorization request; codex_cli_simplified_flow selects the
		// consumer-subscription consent screen instead of the
		// developer/API-key one, and originator identifies the calling
		// tool the same way OpenCode identifies itself as "opencode".
		ExtraAuthParams: map[string]string{
			"id_token_add_organizations": "true",
			"codex_cli_simplified_flow":  "true",
			"originator":                 "chronos-code",
		},
	}
}
