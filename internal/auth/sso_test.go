package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func buildTestJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fakesig"))
	return header + "." + payloadB64 + "." + sig
}

func TestParseJWTClaims(t *testing.T) {
	token := buildTestJWT(map[string]any{
		"sub":    "user-123",
		"email":  "dev@corp.com",
		"name":   "Dev User",
		"iss":    "https://login.corp.com",
		"aud":    "chronos-code",
		"exp":    float64(time.Now().Add(time.Hour).Unix()),
		"iat":    float64(time.Now().Unix()),
		"scope":  "openid profile email",
		"groups": []string{"engineering", "platform"},
	})

	claims, err := parseJWTClaims(token)
	if err != nil {
		t.Fatalf("parseJWTClaims: %v", err)
	}
	if claims.Subject != "user-123" {
		t.Errorf("subject = %q, want user-123", claims.Subject)
	}
	if claims.Email != "dev@corp.com" {
		t.Errorf("email = %q, want dev@corp.com", claims.Email)
	}
	if claims.Issuer != "https://login.corp.com" {
		t.Errorf("issuer = %q, want https://login.corp.com", claims.Issuer)
	}
	if claims.Audience != "chronos-code" {
		t.Errorf("audience = %q, want chronos-code", claims.Audience)
	}
	if len(claims.Scopes) != 3 {
		t.Errorf("scopes = %v, want 3 items", claims.Scopes)
	}
	if len(claims.Groups) != 2 {
		t.Errorf("groups = %v, want 2 items", claims.Groups)
	}
}

func TestParseJWTClaims_AudienceArray(t *testing.T) {
	token := buildTestJWT(map[string]any{
		"sub": "u1",
		"iss": "https://idp.example.com",
		"aud": []string{"chronos-code", "other-app"},
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	claims, err := parseJWTClaims(token)
	if err != nil {
		t.Fatalf("parseJWTClaims: %v", err)
	}
	if claims.Audience != "chronos-code" {
		t.Errorf("audience = %q, want chronos-code (first element)", claims.Audience)
	}
}

func TestParseJWTClaims_MalformedToken(t *testing.T) {
	_, err := parseJWTClaims("not.a.valid-base64!!!")
	if err == nil {
		t.Fatal("expected error for malformed token")
	}
}

func TestParseJWTClaims_TwoSegments(t *testing.T) {
	_, err := parseJWTClaims("only.two")
	if err == nil {
		t.Fatal("expected error for 2-segment token")
	}
}

func TestOIDCValidator_ValidToken(t *testing.T) {
	v, err := NewOIDCValidator(OIDCConfig{
		Issuer:   "https://login.corp.com",
		ClientID: "chronos-code",
	})
	if err != nil {
		t.Fatalf("NewOIDCValidator: %v", err)
	}

	token := buildTestJWT(map[string]any{
		"sub":   "user-1",
		"iss":   "https://login.corp.com",
		"aud":   "chronos-code",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
		"scope": "openid",
	})

	claims, err := v.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.Subject != "user-1" {
		t.Errorf("subject = %q, want user-1", claims.Subject)
	}
}

func TestOIDCValidator_ExpiredToken(t *testing.T) {
	v, _ := NewOIDCValidator(OIDCConfig{
		Issuer:   "https://login.corp.com",
		ClientID: "chronos-code",
	})

	token := buildTestJWT(map[string]any{
		"sub":   "user-1",
		"iss":   "https://login.corp.com",
		"aud":   "chronos-code",
		"exp":   float64(time.Now().Add(-time.Hour).Unix()),
		"scope": "openid",
	})

	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	if got := err.Error(); !contains(got, "expired") {
		t.Errorf("error = %q, want to contain 'expired'", got)
	}
}

func TestOIDCValidator_WrongIssuer(t *testing.T) {
	v, _ := NewOIDCValidator(OIDCConfig{
		Issuer:   "https://login.corp.com",
		ClientID: "chronos-code",
	})

	token := buildTestJWT(map[string]any{
		"sub":   "user-1",
		"iss":   "https://evil.com",
		"aud":   "chronos-code",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
		"scope": "openid",
	})

	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
	if got := err.Error(); !contains(got, "issuer mismatch") {
		t.Errorf("error = %q, want to contain 'issuer mismatch'", got)
	}
}

func TestOIDCValidator_WrongAudience(t *testing.T) {
	v, _ := NewOIDCValidator(OIDCConfig{
		Issuer:   "https://login.corp.com",
		ClientID: "chronos-code",
	})

	token := buildTestJWT(map[string]any{
		"sub":   "user-1",
		"iss":   "https://login.corp.com",
		"aud":   "other-app",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
		"scope": "openid",
	})

	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for wrong audience")
	}
	if got := err.Error(); !contains(got, "audience mismatch") {
		t.Errorf("error = %q, want to contain 'audience mismatch'", got)
	}
}

func TestOIDCValidator_MissingScope(t *testing.T) {
	v, _ := NewOIDCValidator(OIDCConfig{
		Issuer:         "https://login.corp.com",
		ClientID:       "chronos-code",
		RequiredScopes: []string{"openid", "admin"},
	})

	token := buildTestJWT(map[string]any{
		"sub":   "user-1",
		"iss":   "https://login.corp.com",
		"aud":   "chronos-code",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
		"scope": "openid profile",
	})

	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for missing admin scope")
	}
	if got := err.Error(); !contains(got, "missing required scope") {
		t.Errorf("error = %q, want to contain 'missing required scope'", got)
	}
}

func TestOIDCValidator_MissingConfig(t *testing.T) {
	_, err := NewOIDCValidator(OIDCConfig{})
	if err == nil {
		t.Fatal("expected error for empty config")
	}

	_, err = NewOIDCValidator(OIDCConfig{Issuer: "https://x.com"})
	if err == nil {
		t.Fatal("expected error for missing client_id")
	}
}

func TestTokenExchanger_LocalMapping(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "sk-test-123")

	exchanger := NewTokenExchanger(TokenExchangeConfig{
		Mappings: map[string]string{
			"engineering": "anthropic:TEST_ANTHROPIC_KEY",
		},
	})

	claims := &Claims{
		Subject: "user-1",
		Groups:  []string{"engineering"},
	}

	provider, apiKey, err := exchanger.Exchange(claims)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", provider)
	}
	if apiKey != "sk-test-123" {
		t.Errorf("apiKey = %q, want sk-test-123", apiKey)
	}
}

func TestTokenExchanger_EmailFallback(t *testing.T) {
	t.Setenv("TEST_OAI_KEY", "sk-oai-456")

	exchanger := NewTokenExchanger(TokenExchangeConfig{
		Mappings: map[string]string{
			"dev@corp.com": "openai:TEST_OAI_KEY",
		},
	})

	claims := &Claims{
		Subject: "user-2",
		Email:   "dev@corp.com",
		Groups:  []string{"unrelated-group"},
	}

	provider, apiKey, err := exchanger.Exchange(claims)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if provider != "openai" || apiKey != "sk-oai-456" {
		t.Errorf("got (%q, %q), want (openai, sk-oai-456)", provider, apiKey)
	}
}

func TestTokenExchanger_NoMatch(t *testing.T) {
	exchanger := NewTokenExchanger(TokenExchangeConfig{
		Mappings: map[string]string{
			"admin": "anthropic:ADMIN_KEY",
		},
	})

	claims := &Claims{
		Subject: "user-3",
		Groups:  []string{"viewer"},
	}

	_, _, err := exchanger.Exchange(claims)
	if err == nil {
		t.Fatal("expected error for no matching group")
	}
}

func TestTokenExchanger_EmptyEnvVar(t *testing.T) {
	t.Setenv("EMPTY_KEY", "")

	exchanger := NewTokenExchanger(TokenExchangeConfig{
		Mappings: map[string]string{
			"engineering": "anthropic:EMPTY_KEY",
		},
	})

	claims := &Claims{
		Subject: "user-4",
		Groups:  []string{"engineering"},
	}

	_, _, err := exchanger.Exchange(claims)
	if err == nil {
		t.Fatal("expected error for empty env var")
	}
}

func TestTokenExchanger_RemoteEndpoint(t *testing.T) {
	exchanger := NewTokenExchanger(TokenExchangeConfig{
		Endpoint: "https://token-exchange.corp.com/exchange",
	})

	claims := &Claims{Subject: "user-5"}
	_, _, err := exchanger.Exchange(claims)
	if err == nil {
		t.Fatal("expected error for unimplemented remote exchange")
	}
	if got := err.Error(); !contains(got, "not yet implemented") {
		t.Errorf("error = %q, want to contain 'not yet implemented'", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsSubstr(s, sub))
}

func containsSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestScopeArray(t *testing.T) {
	token := buildTestJWT(map[string]any{
		"sub":   "u1",
		"iss":   "https://login.corp.com",
		"aud":   "chronos-code",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
		"scope": []string{"openid", "profile"},
	})

	v, _ := NewOIDCValidator(OIDCConfig{
		Issuer:   "https://login.corp.com",
		ClientID: "chronos-code",
	})

	claims, err := v.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(claims.Scopes) != 2 {
		t.Errorf("scopes = %v, want 2 items", claims.Scopes)
	}
}

func init() {
	// Suppress unused import warning for fmt.
	_ = fmt.Sprint
}
