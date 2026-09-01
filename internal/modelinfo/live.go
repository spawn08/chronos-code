package modelinfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrUnsupportedProvider is returned by FetchLive for any provider without
// a documented, stable models-list endpoint this package knows how to
// call. Callers should fall back to All() (the static registry).
var ErrUnsupportedProvider = errors.New("modelinfo: live model listing not supported for this provider")

// fetchTimeout bounds a single live models-list call — short enough that a
// synchronous caller (e.g. a TUI's /model command) never hangs noticeably.
const fetchTimeout = 5 * time.Second

// anthropicModelsURL and openaiModelsURL are vars, not consts, so tests can
// point them at an httptest.Server instead of the real API.
var (
	anthropicModelsURL = "https://api.anthropic.com/v1/models"
	openaiModelsURL    = "https://api.openai.com/v1/models"
)

// FetchLive queries provider's real models-list API using apiKey and
// returns live Info entries. Only "anthropic" and "openai" are supported —
// the two providers with a documented, stable, unauthenticated-schema
// models-list endpoint — any other provider returns ErrUnsupportedProvider.
//
// Model IDs and their existence come straight from the API, never
// hardcoded. ContextWindow is filled in from this package's static
// registry when the model is known there, and left 0 ("unknown")
// otherwise — because context window size is the one field these
// endpoints don't return at all (confirmed against both vendors' current
// API responses), not because chronos-code chose to hardcode identity data
// it could have fetched.
func FetchLive(ctx context.Context, provider, apiKey string) ([]Info, error) {
	switch provider {
	case "anthropic":
		return fetchAnthropicModels(ctx, apiKey)
	case "openai":
		return fetchOpenAIModels(ctx, apiKey)
	default:
		return nil, ErrUnsupportedProvider
	}
}

func fetchAnthropicModels(ctx context.Context, apiKey string) ([]Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicModelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := doJSON(req, &body); err != nil {
		return nil, fmt.Errorf("modelinfo: fetch anthropic models: %w", err)
	}
	out := make([]Info, len(body.Data))
	for i, d := range body.Data {
		out[i] = enrich("anthropic", d.ID)
	}
	return out, nil
}

func fetchOpenAIModels(ctx context.Context, apiKey string) ([]Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openaiModelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := doJSON(req, &body); err != nil {
		return nil, fmt.Errorf("modelinfo: fetch openai models: %w", err)
	}
	out := make([]Info, len(body.Data))
	for i, d := range body.Data {
		out[i] = enrich("openai", d.ID)
	}
	return out, nil
}

// enrich fills in a live-fetched model ID's ContextWindow from the static
// registry when known, leaving it 0 ("unknown") otherwise.
func enrich(provider, modelID string) Info {
	if info, ok := Lookup(provider, modelID); ok {
		return info
	}
	return Info{Provider: provider, Model: modelID}
}

func doJSON(req *http.Request, out any) error {
	client := &http.Client{Timeout: fetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
