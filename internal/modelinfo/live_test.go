package modelinfo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchLiveAnthropicParsesRealResponseShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q, want test-key", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("anthropic-version header not set")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "claude-sonnet-4-6", "display_name": "Claude Sonnet 4.6"},
				{"id": "claude-brand-new-model", "display_name": "Something not in our static table"},
			},
		})
	}))
	defer srv.Close()

	old := anthropicModelsURL
	anthropicModelsURL = srv.URL
	defer func() { anthropicModelsURL = old }()

	got, err := FetchLive(context.Background(), "anthropic", "test-key")
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Model != "claude-sonnet-4-6" || got[0].ContextWindow != 200_000 {
		t.Errorf("got[0] = %+v, want registry-enriched context window", got[0])
	}
	if got[1].Model != "claude-brand-new-model" || got[1].ContextWindow != 0 {
		t.Errorf("got[1] = %+v, want ContextWindow=0 (unknown) for a model absent from the static registry", got[1])
	}
}

func TestFetchLiveOpenAIParsesRealResponseShape(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", r.Header.Get("Authorization"))
		}
		if requests == 2 && r.URL.Query().Get("after") != "gpt-4o" {
			t.Errorf("after = %q, want gpt-4o", r.URL.Query().Get("after"))
		}
		modelID := "gpt-4o"
		if requests == 2 {
			modelID = "gpt-new"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": modelID, "object": "model"},
			},
			"has_more": requests == 1,
			"last_id":  modelID,
		})
	}))
	defer srv.Close()

	old := openaiModelsURL
	openaiModelsURL = srv.URL
	defer func() { openaiModelsURL = old }()

	got, err := FetchLive(context.Background(), "openai", "test-key")
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	if requests != 2 || len(got) != 2 || got[0].Model != "gpt-4o" || got[0].ContextWindow != 128_000 || got[1].Model != "gpt-new" {
		t.Fatalf("requests=%d got=%+v", requests, got)
	}
}

func TestFetchLiveAzureParsesRealResponseShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("api-key") != "test-key" {
			t.Errorf("api-key = %q, want test-key", r.Header.Get("api-key"))
		}
		if v := r.URL.Query().Get("api-version"); v != "preview" {
			t.Errorf("api-version = %q, want preview", v)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "my-gpt5-deployment", "object": "model"},
			},
		})
	}))
	defer srv.Close()

	t.Setenv("AZURE_OPENAI_ENDPOINT", srv.URL)
	t.Setenv("AZURE_OPENAI_API_VERSION", "preview")

	got, err := FetchLive(context.Background(), "azure", "test-key")
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	if len(got) != 1 || got[0].Provider != "azure" || got[0].Model != "my-gpt5-deployment" {
		t.Fatalf("got = %+v, want [{azure my-gpt5-deployment 0}]", got)
	}
}

func TestFetchLiveAzureWithoutEndpointIsUnsupported(t *testing.T) {
	t.Setenv("AZURE_OPENAI_ENDPOINT", "")
	t.Setenv("AZURE_OPENAI_BASE_URL", "")

	_, err := FetchLive(context.Background(), "azure", "test-key")
	if !errors.Is(err, ErrMissingEndpoint) {
		t.Fatalf("err = %v, want ErrMissingEndpoint", err)
	}
}

func TestFetchLiveUnsupportedProvider(t *testing.T) {
	_, err := FetchLive(context.Background(), "some-custom-provider", "key")
	if err != ErrUnsupportedProvider {
		t.Fatalf("err = %v, want ErrUnsupportedProvider", err)
	}
}

func TestFetchLiveHTTPErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad-key rejected"}`))
	}))
	defer srv.Close()

	old := anthropicModelsURL
	anthropicModelsURL = srv.URL
	defer func() { anthropicModelsURL = old }()

	_, err := FetchLive(context.Background(), "anthropic", "bad-key")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		t.Fatal("FetchLive: want error for a 401 response")
	}
	if strings.Contains(err.Error(), "bad-key") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("HTTP error did not safely redact its bounded snippet: %v", err)
	}
}

func TestFetchLiveUsesConfiguredBaseURLAndPaginates(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/gateway/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if requests == 1 {
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "one"}}, "has_more": true, "last_id": "cursor-1"})
			return
		}
		if got := r.URL.Query().Get("after_id"); got != "cursor-1" {
			t.Errorf("after_id = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "two"}}, "has_more": false})
	}))
	defer srv.Close()

	got, err := FetchLive(context.Background(), " Claude ", "key", LiveConfig{BaseURL: srv.URL + "/gateway"})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(got) != 2 || got[0].Provider != "anthropic" || got[1].Model != "two" {
		t.Fatalf("requests=%d models=%+v", requests, got)
	}
}

func TestFetchLiveAzureBaseURLAlias(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "deployment"}}})
	}))
	defer srv.Close()
	t.Setenv("AZURE_OPENAI_ENDPOINT", "")
	t.Setenv("AZURE_OPENAI_BASE_URL", srv.URL)
	got, err := FetchLive(context.Background(), "azure-openai", "key")
	if err != nil || len(got) != 1 || got[0].Provider != "azure" {
		t.Fatalf("FetchLive() = %+v, %v", got, err)
	}
}
