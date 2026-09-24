package modelinfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var ErrUnsupportedProvider = errors.New("modelinfo: live model listing not supported for this provider")
var ErrMissingEndpoint = errors.New("modelinfo: provider endpoint is not configured")

type LiveConfig struct {
	BaseURL  string
	Endpoint string
}

type ConfigurationError struct {
	Provider string
	Err      error
}

func (e *ConfigurationError) Error() string {
	return fmt.Sprintf("modelinfo: %s configuration: %v", e.Provider, e.Err)
}
func (e *ConfigurationError) Unwrap() error { return e.Err }

type HTTPError struct {
	Provider   string
	StatusCode int
	Snippet    string
}

type RequestError struct {
	Provider string
	Err      error
}

func (e *RequestError) Error() string {
	switch {
	case errors.Is(e.Err, context.DeadlineExceeded):
		return fmt.Sprintf("modelinfo: %s models request timed out", e.Provider)
	case errors.Is(e.Err, context.Canceled):
		return fmt.Sprintf("modelinfo: %s models request canceled", e.Provider)
	default:
		return fmt.Sprintf("modelinfo: %s models request failed", e.Provider)
	}
}
func (e *RequestError) Unwrap() error { return e.Err }

func (e *HTTPError) Error() string {
	if e.Snippet == "" {
		return fmt.Sprintf("modelinfo: %s models returned HTTP %d", e.Provider, e.StatusCode)
	}
	return fmt.Sprintf("modelinfo: %s models returned HTTP %d: %s", e.Provider, e.StatusCode, e.Snippet)
}

func LiveProviders() []string { return []string{"anthropic", "openai", "azure"} }

const fetchTimeout = 5 * time.Second

var (
	anthropicModelsURL = "https://api.anthropic.com/v1/models"
	openaiModelsURL    = "https://api.openai.com/v1/models"
)

func CanonicalProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return "anthropic"
	case "codex":
		return "openai"
	case "azure-openai":
		return "azure"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

// FetchLive performs an opt-in live lookup. The optional configuration routes
// discovery through the same base URL or endpoint used for model calls.
func FetchLive(ctx context.Context, provider, apiKey string, configs ...LiveConfig) ([]Info, error) {
	provider = CanonicalProvider(provider)
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fetchTimeout)
		defer cancel()
	}
	var cfg LiveConfig
	if len(configs) > 0 {
		cfg = configs[0]
	}
	switch provider {
	case "anthropic":
		endpoint, err := modelsURL(cfg.BaseURL, anthropicModelsURL)
		if err != nil {
			return nil, &ConfigurationError{Provider: provider, Err: err}
		}
		return fetchPagedModels(ctx, provider, endpoint, apiKey, "after_id")
	case "openai":
		endpoint, err := modelsURL(cfg.BaseURL, openaiModelsURL)
		if err != nil {
			return nil, &ConfigurationError{Provider: provider, Err: err}
		}
		return fetchPagedModels(ctx, provider, endpoint, apiKey, "after")
	case "azure":
		return fetchAzureModels(ctx, apiKey, cfg)
	default:
		return nil, ErrUnsupportedProvider
	}
}

func modelsURL(baseURL, fallback string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		return fallback, nil
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	endpoint := baseURL + "/v1/models"
	if strings.HasSuffix(baseURL, "/models") {
		endpoint = baseURL
	}
	if strings.HasSuffix(baseURL, "/v1") {
		endpoint = baseURL + "/models"
	}
	if !validHTTPURL(endpoint) {
		return "", errors.New("invalid models URL")
	}
	return endpoint, nil
}

type modelsPage struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	LastID  string `json:"last_id"`
}

func fetchPagedModels(ctx context.Context, provider, endpoint, apiKey, cursorParam string) ([]Info, error) {
	var out []Info
	cursor := ""
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, &ConfigurationError{Provider: provider, Err: errors.New("invalid models URL")}
		}
		if cursor != "" {
			query := u.Query()
			query.Set(cursorParam, cursor)
			u.RawQuery = query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		if provider == "anthropic" {
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		var page modelsPage
		if err := doJSON(provider, apiKey, req, &page); err != nil {
			return nil, fmt.Errorf("modelinfo: fetch %s models: %w", provider, err)
		}
		for _, item := range page.Data {
			if item.ID != "" {
				out = append(out, enrich(provider, item.ID))
			}
		}
		if !page.HasMore {
			return out, nil
		}
		if page.LastID == "" || page.LastID == cursor {
			return nil, fmt.Errorf("modelinfo: fetch %s models: pagination cursor missing or repeated", provider)
		}
		cursor = page.LastID
	}
	return nil, fmt.Errorf("modelinfo: fetch %s models: pagination exceeded 100 pages", provider)
}

func fetchAzureModels(ctx context.Context, apiKey string, cfg LiveConfig) ([]Info, error) {
	endpoint := firstNonEmpty(cfg.Endpoint, cfg.BaseURL, os.Getenv("AZURE_OPENAI_ENDPOINT"), os.Getenv("AZURE_OPENAI_BASE_URL"))
	endpoint = strings.TrimRight(endpoint, "/")
	if endpoint == "" {
		return nil, &ConfigurationError{Provider: "azure", Err: ErrMissingEndpoint}
	}
	apiVersion := os.Getenv("AZURE_OPENAI_API_VERSION")
	if apiVersion == "" {
		apiVersion = "preview"
	}
	modelsEndpoint := endpoint
	if !strings.HasSuffix(modelsEndpoint, "/models") {
		if strings.HasSuffix(modelsEndpoint, "/openai/v1") {
			modelsEndpoint += "/models"
		} else {
			modelsEndpoint += "/openai/v1/models"
		}
	}
	if !validHTTPURL(modelsEndpoint) {
		return nil, &ConfigurationError{Provider: "azure", Err: errors.New("invalid endpoint")}
	}
	u, err := url.Parse(modelsEndpoint)
	if err != nil {
		return nil, &ConfigurationError{Provider: "azure", Err: errors.New("invalid endpoint")}
	}
	query := u.Query()
	query.Set("api-version", apiVersion)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("api-key", apiKey)
	var body modelsPage
	if err := doJSON("azure", apiKey, req, &body); err != nil {
		return nil, fmt.Errorf("modelinfo: fetch azure models: %w", err)
	}
	out := make([]Info, 0, len(body.Data))
	for _, item := range body.Data {
		if item.ID != "" {
			out = append(out, enrich("azure", item.ID))
		}
	}
	return out, nil
}

func enrich(provider, modelID string) Info {
	if info, ok := Lookup(provider, modelID); ok {
		return info
	}
	return Info{Provider: provider, Model: modelID}
}

func doJSON(provider, apiKey string, req *http.Request, out any) error {
	resp, err := (&http.Client{Timeout: fetchTimeout}).Do(req)
	if err != nil {
		return &RequestError{Provider: provider, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 513))
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		if apiKey != "" {
			snippet = strings.ReplaceAll(snippet, apiKey, "[REDACTED]")
		}
		return &HTTPError{Provider: provider, StatusCode: resp.StatusCode, Snippet: snippet}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func validHTTPURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
