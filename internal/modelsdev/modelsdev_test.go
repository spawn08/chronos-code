package modelsdev

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/modelinfo"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "api.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parsedFixture(t *testing.T) Catalog {
	t.Helper()
	catalog, err := Parse(fixture(t))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return catalog
}

// resetGlobals removes every process-wide layer Apply installs.
func resetGlobals(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		Apply(nil)
		if err := budget.LoadPricing(); err != nil {
			t.Errorf("reset pricing: %v", err)
		}
	})
}

func TestParseKeepsOnlySupportedProviders(t *testing.T) {
	catalog := parsedFixture(t)
	if _, ok := catalog["somecloud"]; ok {
		t.Error("unsupported provider kept")
	}
	for _, id := range []string{"anthropic", "openai", "google", "groq", "openrouter"} {
		if _, ok := catalog[id]; !ok {
			t.Errorf("supported provider %q dropped", id)
		}
	}
	for _, bad := range []string{"<html>rate limited</html>", `{"somecloud":{"models":{"x":{}}}}`, `{}`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%q) error = nil, want rejection", bad)
		}
	}
}

func TestCatalogPrices(t *testing.T) {
	prices := parsedFixture(t).prices()

	sonnet := prices["claude-sonnet-5"]
	want := budget.Rates{InputPerMillion: 2_000_000, OutputPerMillion: 10_000_000, CacheReadPerMillion: 200_000,
		CacheWritePerMillion: 2_500_000, CacheWrite1hPerMillion: 4_000_000}
	if sonnet.Rates != want {
		t.Errorf("claude-sonnet-5 = %+v, want %+v (1h write = 2x input)", sonnet.Rates, want)
	}
	// First-party providers win over aggregators hosting the same ID.
	if got := prices["gpt-5"].InputPerMillion; got != 1_250_000 {
		t.Errorf("gpt-5 input = %d, want openai's 1250000 not groq's", got)
	}
	// Explicit tiers win over context_over_200k.
	if tiers := prices["gpt-5.5"].Tiers; len(tiers) != 1 || tiers[0].AbovePromptTokens != 272_000 || tiers[0].InputPerMillion != 10_000_000 {
		t.Errorf("gpt-5.5 tiers = %+v, want one tier above 272000", tiers)
	}
	// context_over_200k becomes a tier when no tiers are published.
	if tiers := prices["gemini-9-pro"].Tiers; len(tiers) != 1 || tiers[0].AbovePromptTokens != 200_000 || tiers[0].OutputPerMillion != 15_000_000 {
		t.Errorf("gemini-9-pro tiers = %+v, want context_over_200k tier", tiers)
	}
	// Aggregator IDs keep their provider prefix.
	if _, ok := prices["anthropic/claude-sonnet-4.5"]; !ok {
		t.Error("openrouter model not priced under its full ID")
	}
	for _, unpriced := range []string{"free-model", "text-embedding-9", "mystery"} {
		if _, ok := prices[unpriced]; ok {
			t.Errorf("%q priced, want unpriced (zero price or unsupported provider)", unpriced)
		}
	}
}

func TestCatalogContextLimitsAndPickerEntries(t *testing.T) {
	catalog := parsedFixture(t)
	limits := catalog.contextLimits()
	for id, want := range map[string]int{"gpt-5": 272_000, "claude-sonnet-5": 1_000_000, "gemini-9-pro": 1_048_576} {
		if got := limits[id]; got != want {
			t.Errorf("contextLimits[%q] = %d, want %d", id, got, want)
		}
	}

	infos := catalog.modelInfos()
	has := func(provider, id string) bool {
		for _, info := range infos {
			if info.Provider == provider && info.Model == id {
				return true
			}
		}
		return false
	}
	if !has("gemini", "gemini-9-pro") {
		t.Error("google model not mapped to the chronos gemini provider")
	}
	if has("anthropic", "claude-retired-1") || has("openai", "text-embedding-9") {
		t.Error("deprecated or non-tool-calling model offered in the picker")
	}
	if stats := catalog.Stats(); stats.Providers != 5 || stats.Models != len(infos) {
		t.Errorf("Stats() = %+v", stats)
	}
}

func TestApplyWiresPricesContextAndPickerAndOverlaysStillWin(t *testing.T) {
	resetGlobals(t)
	Apply(parsedFixture(t))

	if got, ok := model.KnownContextLimit("gemini-9-pro"); !ok || got != 1_048_576 {
		t.Errorf("KnownContextLimit(gemini-9-pro) = %d, %v", got, ok)
	}
	// Normalized IDs resolve through the same candidates as prices.
	if got, ok := model.KnownContextLimit("openai/gpt-5"); !ok || got != 272_000 {
		t.Errorf("KnownContextLimit(openai/gpt-5) = %d, %v", got, ok)
	}
	if info, ok := modelinfo.Lookup("gemini", "gemini-9-pro"); !ok || info.ContextWindow != 1_048_576 {
		t.Errorf("modelinfo.Lookup(gemini-9-pro) = %+v, %v", info, ok)
	}
	if info, ok := modelinfo.LookupByModel("gemini-9-pro"); !ok || info.Provider != "gemini" {
		t.Errorf("LookupByModel(gemini-9-pro) = %+v, %v", info, ok)
	}
	if price, err := budget.PriceForModel("gemini-9-pro"); err != nil || price.InputPerMillion != 1_250_000 {
		t.Errorf("PriceForModel(gemini-9-pro) = %+v, %v", price, err)
	}
	// Catalog beats bundled pricing.yaml...
	if price, _ := budget.PriceForModel("gpt-5.5"); len(price.Tiers) != 1 {
		t.Errorf("gpt-5.5 tiers = %+v", price.Tiers)
	}
	// ...but a user/project overlay beats the catalog, in either load order.
	if err := budget.LoadPricing(budget.PricingOverlay{Source: "project", Data: []byte("models:\n  claude-sonnet-5: { input: 7, output: 7 }\n")}); err != nil {
		t.Fatal(err)
	}
	Apply(parsedFixture(t))
	if price, _ := budget.PriceForModel("claude-sonnet-5"); price.InputPerMillion != 7_000_000 {
		t.Errorf("overlay lost to catalog: %+v", price.Rates)
	}

	Apply(nil)
	if _, ok := modelinfo.Lookup("gemini", "gemini-9-pro"); ok {
		t.Error("Apply(nil) left catalog models in the picker")
	}
	if _, err := budget.PriceForModel("gemini-9-pro"); err == nil {
		t.Error("Apply(nil) left catalog prices active")
	}
}

func catalogServer(t *testing.T, status int, body []byte) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var hits atomic.Int32
	var userAgent atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		userAgent.Store(r.Header.Get("User-Agent"))
		if r.URL.Path != "/api.json" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &userAgent
}

func TestRefreshFetchesSavesAndApplies(t *testing.T) {
	resetGlobals(t)
	srv, _, userAgent := catalogServer(t, http.StatusOK, fixture(t))
	path := filepath.Join(t.TempDir(), "cache", "models.json")

	catalog, err := Refresh(context.Background(), Options{CachePath: path, URL: srv.URL + "/", UserAgent: "chronos-code/test"})
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := userAgent.Load(); got != "chronos-code/test" {
		t.Errorf("User-Agent = %v", got)
	}
	loaded, _, err := Load(path)
	if err != nil || len(loaded) != len(catalog) {
		t.Fatalf("Load() after Refresh = %d providers, %v", len(loaded), err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("cache dir has %d entries, want only models.json (no temp files)", len(entries))
	}
	if _, ok := loaded["somecloud"]; ok {
		t.Error("cache not trimmed to supported providers")
	}
	if _, err := budget.PriceForModel("gemini-9-pro"); err != nil {
		t.Errorf("Refresh did not apply the catalog: %v", err)
	}
}

func TestRefreshFailureKeepsExistingCache(t *testing.T) {
	resetGlobals(t)
	srv, _, _ := catalogServer(t, http.StatusServiceUnavailable, []byte("down"))
	path := filepath.Join(t.TempDir(), "models.json")
	if err := Save(path, parsedFixture(t)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err := Refresh(context.Background(), Options{CachePath: path, URL: srv.URL}); err == nil {
		t.Fatal("Refresh() error = nil for HTTP 503")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("failed refresh modified the cache")
	}
}

func TestInitRefreshPolicy(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		cacheAge    time.Duration // <0: no cache
		autoRefresh bool
		disableEnv  string
		wantFetch   bool
	}{
		{name: "missing cache", cacheAge: -1, autoRefresh: true, wantFetch: true},
		{name: "fresh cache", cacheAge: time.Hour, autoRefresh: true, wantFetch: false},
		{name: "stale cache", cacheAge: 25 * time.Hour, autoRefresh: true, wantFetch: true},
		{name: "auto refresh off", cacheAge: -1, autoRefresh: false, wantFetch: false},
		{name: "env opt-out", cacheAge: -1, autoRefresh: true, disableEnv: "1", wantFetch: false},
		{name: "env false keeps refresh", cacheAge: -1, autoRefresh: true, disableEnv: "false", wantFetch: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetGlobals(t)
			t.Setenv(DisableFetchEnv, tt.disableEnv)
			srv, hits, _ := catalogServer(t, http.StatusOK, fixture(t))
			path := filepath.Join(t.TempDir(), "models.json")
			if tt.cacheAge >= 0 {
				if err := Save(path, parsedFixture(t)); err != nil {
					t.Fatal(err)
				}
				mtime := now.Add(-tt.cacheAge)
				if err := os.Chtimes(path, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}
			done, err := Init(context.Background(), Options{
				CachePath: path, URL: srv.URL, AutoRefresh: tt.autoRefresh, Now: func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if tt.cacheAge >= 0 {
				// A cached catalog is applied before any network work.
				if _, err := budget.PriceForModel("gemini-9-pro"); err != nil {
					t.Errorf("cached catalog not applied synchronously: %v", err)
				}
			}
			if bgErr := <-done; bgErr != nil {
				t.Fatalf("background refresh error = %v", bgErr)
			}
			if fetched := hits.Load() > 0; fetched != tt.wantFetch {
				t.Errorf("fetched = %v, want %v", fetched, tt.wantFetch)
			}
		})
	}
}

func TestInitReportsCorruptCacheAndRefreshesIt(t *testing.T) {
	resetGlobals(t)
	t.Setenv(DisableFetchEnv, "")
	srv, _, _ := catalogServer(t, http.StatusOK, fixture(t))
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	done, err := Init(context.Background(), Options{CachePath: path, URL: srv.URL, AutoRefresh: true})
	if err == nil {
		t.Error("Init() error = nil for a corrupt cache")
	}
	if bgErr := <-done; bgErr != nil {
		t.Fatalf("background refresh error = %v", bgErr)
	}
	if _, _, err := Load(path); err != nil {
		t.Errorf("corrupt cache not replaced: %v", err)
	}
}
