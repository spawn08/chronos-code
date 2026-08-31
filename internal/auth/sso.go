package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// OIDCConfig holds configuration for OIDC-based SSO validation.
type OIDCConfig struct {
	Issuer         string   `yaml:"issuer" json:"issuer"`
	ClientID       string   `yaml:"client_id" json:"client_id"`
	RequiredScopes []string `yaml:"required_scopes,omitempty" json:"required_scopes,omitempty"`
}

// Claims are the validated identity fields extracted from an OIDC JWT.
type Claims struct {
	Subject   string   `json:"sub"`
	Email     string   `json:"email"`
	Name      string   `json:"name"`
	Groups    []string `json:"groups"`
	Scopes    []string `json:"scope"`
	Issuer    string   `json:"iss"`
	Audience  string   `json:"aud"`
	ExpiresAt int64    `json:"exp"`
	IssuedAt  int64    `json:"iat"`
}

// OIDCValidator validates OIDC JWT tokens for server-mode authentication.
// It performs claim-level validation (issuer, audience, expiry, scopes).
//
// Signature verification via JWKS is not yet implemented — the validator
// trusts the claims after structural + claim checks. This is suitable for
// internal/trusted networks; production deployments behind a reverse proxy
// that terminates OIDC (e.g., Envoy, Istio, oauth2-proxy) are unaffected
// since the proxy already verified the signature.
type OIDCValidator struct {
	config OIDCConfig
	mu     sync.RWMutex
}

// NewOIDCValidator creates a validator for the given OIDC configuration.
func NewOIDCValidator(cfg OIDCConfig) (*OIDCValidator, error) {
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("sso: issuer is required")
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("sso: client_id is required")
	}
	if len(cfg.RequiredScopes) == 0 {
		cfg.RequiredScopes = []string{"openid"}
	}
	return &OIDCValidator{config: cfg}, nil
}

// Validate parses a JWT token string, verifies claims (issuer, audience,
// expiry, required scopes), and returns the extracted claims.
func (v *OIDCValidator) Validate(tokenString string) (*Claims, error) {
	claims, err := parseJWTClaims(tokenString)
	if err != nil {
		return nil, fmt.Errorf("sso: %w", err)
	}

	// TODO: implement JWKS signature verification — fetch keys from
	// v.config.Issuer + "/.well-known/openid-configuration", cache them,
	// and verify the JWT signature (RS256/ES256) against the matching kid.

	if claims.Issuer != v.config.Issuer {
		return nil, fmt.Errorf("sso: issuer mismatch: got %q, want %q", claims.Issuer, v.config.Issuer)
	}

	if claims.Audience != v.config.ClientID {
		return nil, fmt.Errorf("sso: audience mismatch: got %q, want %q", claims.Audience, v.config.ClientID)
	}

	if claims.ExpiresAt > 0 && time.Now().Unix() > claims.ExpiresAt {
		return nil, fmt.Errorf("sso: token expired at %s", time.Unix(claims.ExpiresAt, 0).Format(time.RFC3339))
	}

	for _, required := range v.config.RequiredScopes {
		if !containsScope(claims.Scopes, required) {
			return nil, fmt.Errorf("sso: missing required scope %q", required)
		}
	}

	return claims, nil
}

// parseJWTClaims decodes the payload segment of a JWT (header.payload.signature)
// without verifying the signature.
func parseJWTClaims(tokenString string) (*Claims, error) {
	parts := strings.SplitN(tokenString, ".", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed JWT: expected 3 segments, got %d", len(parts))
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode JWT payload: %w", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("parse JWT payload: %w", err)
	}

	var claims Claims

	unmarshalString(raw, "sub", &claims.Subject)
	unmarshalString(raw, "email", &claims.Email)
	unmarshalString(raw, "name", &claims.Name)
	unmarshalString(raw, "iss", &claims.Issuer)
	unmarshalInt64(raw, "exp", &claims.ExpiresAt)
	unmarshalInt64(raw, "iat", &claims.IssuedAt)

	if audRaw, ok := raw["aud"]; ok {
		var single string
		if json.Unmarshal(audRaw, &single) == nil {
			claims.Audience = single
		} else {
			var multi []string
			if json.Unmarshal(audRaw, &multi) == nil && len(multi) > 0 {
				claims.Audience = multi[0]
			}
		}
	}

	if scopeRaw, ok := raw["scope"]; ok {
		var scopeStr string
		if json.Unmarshal(scopeRaw, &scopeStr) == nil {
			claims.Scopes = strings.Fields(scopeStr)
		} else {
			var scopeArr []string
			if json.Unmarshal(scopeRaw, &scopeArr) == nil {
				claims.Scopes = scopeArr
			}
		}
	}

	if groupsRaw, ok := raw["groups"]; ok {
		_ = json.Unmarshal(groupsRaw, &claims.Groups)
	}

	return &claims, nil
}

func unmarshalString(m map[string]json.RawMessage, key string, dst *string) {
	if raw, ok := m[key]; ok {
		_ = json.Unmarshal(raw, dst)
	}
}

func unmarshalInt64(m map[string]json.RawMessage, key string, dst *int64) {
	if raw, ok := m[key]; ok {
		var f float64
		if json.Unmarshal(raw, &f) == nil {
			*dst = int64(f)
		}
	}
}

func containsScope(scopes []string, target string) bool {
	for _, s := range scopes {
		if s == target {
			return true
		}
	}
	return false
}

// TokenExchangeConfig maps SSO identities to model provider credentials.
type TokenExchangeConfig struct {
	// Endpoint is the URL of a remote token exchange service. When set,
	// Exchange posts the SSO claims and receives a provider API key.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// Mappings maps group/role names to "provider:ENV_VAR" pairs. The env
	// var is resolved locally to obtain the API key.
	Mappings map[string]string `yaml:"mappings,omitempty" json:"mappings,omitempty"`
}

// TokenExchanger maps validated SSO claims to provider API keys by matching
// the user's group memberships against configured mappings.
type TokenExchanger struct {
	config TokenExchangeConfig
}

// NewTokenExchanger creates a TokenExchanger from the given configuration.
func NewTokenExchanger(cfg TokenExchangeConfig) *TokenExchanger {
	return &TokenExchanger{config: cfg}
}

// Exchange resolves a provider and API key for the given SSO claims. It
// checks the user's groups against the mapping config. The first matching
// group wins. When no endpoint is configured, the API key is resolved from
// a local environment variable.
func (e *TokenExchanger) Exchange(claims *Claims) (provider, apiKey string, err error) {
	if e.config.Endpoint != "" {
		// TODO: implement remote token exchange — POST claims to endpoint,
		// receive {provider, api_key} response.
		return "", "", fmt.Errorf("sso: remote token exchange not yet implemented")
	}

	for _, group := range claims.Groups {
		mapping, ok := e.config.Mappings[group]
		if !ok {
			continue
		}
		parts := strings.SplitN(mapping, ":", 2)
		if len(parts) != 2 {
			continue
		}
		provider = parts[0]
		envVar := parts[1]
		apiKey = os.Getenv(envVar)
		if apiKey == "" {
			return "", "", fmt.Errorf("sso: env var %q for group %q is empty", envVar, group)
		}
		return provider, apiKey, nil
	}

	if claims.Email != "" {
		mapping, ok := e.config.Mappings[claims.Email]
		if ok {
			parts := strings.SplitN(mapping, ":", 2)
			if len(parts) == 2 {
				provider = parts[0]
				apiKey = os.Getenv(parts[1])
				if apiKey != "" {
					return provider, apiKey, nil
				}
			}
		}
	}

	return "", "", fmt.Errorf("sso: no provider mapping found for groups %v", claims.Groups)
}
