package modelinfo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "gpt-4o", "object": "model"},
			},
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
	if len(got) != 1 || got[0].Model != "gpt-4o" || got[0].ContextWindow != 128_000 {
		t.Fatalf("got = %+v, want [{openai gpt-4o 128000}]", got)
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
	}))
	defer srv.Close()

	old := anthropicModelsURL
	anthropicModelsURL = srv.URL
	defer func() { anthropicModelsURL = old }()

	if _, err := FetchLive(context.Background(), "anthropic", "bad-key"); err == nil {
		t.Fatal("FetchLive: want error for a 401 response")
	}
}
