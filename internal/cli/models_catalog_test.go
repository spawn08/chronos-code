package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/modelsdev"
)

const refreshFixture = `{"anthropic":{"id":"anthropic","name":"Anthropic","models":{
  "claude-test-9":{"id":"claude-test-9","name":"Test","tool_call":true,
    "limit":{"context":500000,"output":8000},"cost":{"input":2,"output":10}}}}}`

func TestRunModelsRefreshCommandWritesCacheAndReports(t *testing.T) {
	t.Cleanup(func() { modelsdev.Apply(nil) })
	var userAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(refreshFixture))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "cache", "models.json")

	var out bytes.Buffer
	// An explicit refresh ignores the automatic-fetch opt-out set in TestMain.
	err := runModelsRefreshCommand(context.Background(), modelsdev.Options{CachePath: path, URL: srv.URL, UserAgent: "chronos-code/test"}, &out)
	if err != nil {
		t.Fatalf("runModelsRefreshCommand() error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "1 providers, 1 models, 1 priced") || !strings.Contains(got, path) {
		t.Errorf("output = %q", got)
	}
	if userAgent != "chronos-code/test" {
		t.Errorf("User-Agent = %q", userAgent)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("cache not written: %v", err)
	}
	if _, err := budget.PriceForModel("claude-test-9"); err != nil {
		t.Errorf("refreshed catalog not applied: %v", err)
	}
}

func TestModelsCatalogOptionsFollowConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	disabled := false
	cfg := &config.Config{Models: config.ModelsCatalogConfig{AutoRefresh: &disabled, URL: "https://mirror.example/"}}
	opts, err := modelsCatalogOptions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if opts.AutoRefresh || opts.URL != "https://mirror.example" {
		t.Errorf("options = %+v, want refresh off and trimmed mirror URL", opts)
	}
	if want := filepath.Join(home, config.ConfigDirName, "cache", "models.json"); opts.CachePath != want {
		t.Errorf("CachePath = %q, want %q", opts.CachePath, want)
	}
	if defaults := (config.ModelsCatalogConfig{}); !defaults.AutoRefreshEnabled() || defaults.SourceURL() != config.DefaultModelsCatalogURL {
		t.Errorf("zero config = refresh %v, url %q", defaults.AutoRefreshEnabled(), defaults.SourceURL())
	}
}
