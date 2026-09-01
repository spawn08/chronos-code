package auth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// jwksCache holds fetched JWKS keys with a TTL.
type jwksCache struct {
	keys      map[string]*rsa.PublicKey // kid -> public key
	fetchedAt time.Time
	ttl       time.Duration
}

func (c *jwksCache) expired() bool {
	return c == nil || time.Since(c.fetchedAt) > c.ttl
}

// fetchJWKS discovers the JWKS URI from the issuer's OpenID configuration
// and fetches the key set. Only RSA keys (kty: "RSA") are extracted.
func fetchJWKS(issuer string) (*jwksCache, error) {
	wellKnown := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	jwksURI, err := discoverJWKSURI(wellKnown)
	if err != nil {
		return nil, fmt.Errorf("jwks: discover: %w", err)
	}

	keys, err := fetchAndParseJWKS(jwksURI)
	if err != nil {
		return nil, fmt.Errorf("jwks: fetch: %w", err)
	}

	return &jwksCache{
		keys:      keys,
		fetchedAt: time.Now(),
		ttl:       1 * time.Hour,
	}, nil
}

func discoverJWKSURI(wellKnownURL string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(wellKnownURL)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", wellKnownURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", wellKnownURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("parse discovery doc: %w", err)
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("no jwks_uri in discovery document")
	}
	return doc.JWKSURI, nil
}

type jwksResponse struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	KTY string `json:"kty"`
	Use string `json:"use"`
	KID string `json:"kid"`
	ALG string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func fetchAndParseJWKS(jwksURI string) (map[string]*rsa.PublicKey, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(jwksURI)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", jwksURI, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", jwksURI, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var jwks jwksResponse
	if err := json.Unmarshal(body, &jwks); err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey)
	for _, k := range jwks.Keys {
		if k.KTY != "RSA" {
			continue
		}
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := parseRSAPublicKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.KID] = pub
	}
	return keys, nil
}

func parseRSAPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	n := new(big.Int).SetBytes(nBytes)
	e := 0
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}

// verifyRS256 verifies an RS256 JWT signature. signingInput is
// "header.payload" (the first two base64url segments), sigB64 is the
// third segment (the signature).
func verifyRS256(key *rsa.PublicKey, signingInput, sigB64 string) error {
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	hash := sha256.Sum256([]byte(signingInput))
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, hash[:], sig)
}
