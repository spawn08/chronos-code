// Package modelsdev loads the models.dev catalog (https://models.dev), the
// open model database opencode also uses, and applies it to chronos-code:
// model prices (budget), context windows (chronos model.KnownContextLimit),
// and /model picker entries (modelinfo).
//
// The design follows opencode's ModelsDev service: a disk cache of the
// catalog that is loaded synchronously, refreshed over the network in the
// background when stale, and written atomically. Unlike opencode, the cache
// is trimmed to the providers chronos-code can build, and the offline
// fallback is the bundled pricing.yaml / SDK tables rather than an embedded
// snapshot.
package modelsdev

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spawn08/chronos/engine/model"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/modelid"
	"github.com/spawn08/chronos-code/internal/modelinfo"
)

const (
	// CacheTTL is how old the cache may be before a background refresh.
	CacheTTL = 24 * time.Hour
	// DisableFetchEnv turns off automatic (not explicitly requested) fetches.
	DisableFetchEnv = "CHRONOS_CODE_DISABLE_MODELS_FETCH"

	fetchTimeout = 20 * time.Second
	// maxCatalogBytes bounds the download; api.json is ~5 MB today.
	maxCatalogBytes = 64 << 20
)

// providerMapping maps models.dev provider IDs to chronos provider names.
// Order is precedence when a model ID appears under several providers:
// first-party providers win over aggregators for prices and context windows.
var providerMapping = []struct{ catalog, chronos string }{
	{"anthropic", "anthropic"},
	{"openai", "openai"},
	{"google", "gemini"},
	{"mistral", "mistral"},
	{"deepseek", "deepseek"},
	{"groq", "groq"},
	{"togetherai", "together"},
	{"fireworks-ai", "fireworks"},
	{"perplexity", "perplexity"},
	{"openrouter", "openrouter"},
}

// Catalog is the subset of models.dev api.json chronos-code uses, keyed by
// models.dev provider ID.
type Catalog map[string]Provider

// Provider is one models.dev provider.
type Provider struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Models map[string]Model `json:"models"`
}

// Model is one models.dev model entry.
type Model struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ToolCall bool   `json:"tool_call"`
	Status   string `json:"status,omitempty"`
	Limit    Limit  `json:"limit"`
	Cost     *Cost  `json:"cost,omitempty"`
}

// Limit is a model's token limits. Input, when set, is the usable prompt
// window and is smaller than Context (e.g. gpt-5: 272K of 400K).
type Limit struct {
	Context float64 `json:"context"`
	Input   float64 `json:"input,omitempty"`
	Output  float64 `json:"output"`
}

// Rates are USD per 1M tokens.
type Rates struct {
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

// Cost is a model's pricing, with optional long-context tiers.
type Cost struct {
	Rates
	Tiers           []CostTier `json:"tiers,omitempty"`
	ContextOver200K *Rates     `json:"context_over_200k,omitempty"`
}

// CostTier applies once the prompt exceeds Tier.Size tokens.
type CostTier struct {
	Rates
	Tier struct {
		Type string  `json:"type"`
		Size float64 `json:"size"`
	} `json:"tier"`
}

// CachePath is the catalog cache location under the user config directory.
func CachePath(userDir string) string {
	return filepath.Join(userDir, "cache", "models.json")
}

// Parse decodes models.dev api.json (or a trimmed cache), keeping only the
// providers chronos-code supports. It fails if none are present, which also
// rejects error pages and unexpected schema changes.
func Parse(data []byte) (Catalog, error) {
	var raw map[string]Provider
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	catalog := make(Catalog, len(providerMapping))
	for _, mapping := range providerMapping {
		if provider, ok := raw[mapping.catalog]; ok && len(provider.Models) > 0 {
			catalog[mapping.catalog] = provider
		}
	}
	if len(catalog) == 0 {
		return nil, errors.New("models.dev catalog contains none of the supported providers")
	}
	return catalog, nil
}

// Load reads and parses the cache at path, returning its modification time.
func Load(path string) (Catalog, time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	catalog, err := Parse(data)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%s: %w", path, err)
	}
	return catalog, info.ModTime(), nil
}

// Fetch downloads <baseURL>/api.json and parses it.
func Fetch(ctx context.Context, client *http.Client, baseURL, userAgent string) (Catalog, error) {
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fetchTimeout)
		defer cancel()
	}
	url := strings.TrimRight(baseURL, "/") + "/api.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build models.dev request: %w", err)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if len(data) > maxCatalogBytes {
		return nil, fmt.Errorf("fetch %s: catalog exceeds %d bytes", url, maxCatalogBytes)
	}
	return Parse(data)
}

// Save writes the trimmed catalog to path atomically (temp file + rename in
// the same directory), so a concurrent reader or an interrupted write never
// sees a partial file.
func Save(path string, catalog Catalog) error {
	data, err := json.Marshal(catalog)
	if err != nil {
		return fmt.Errorf("encode models.dev catalog: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp catalog: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp catalog: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp catalog: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// Stale reports whether a cache written at modTime should be refreshed.
func Stale(modTime, now time.Time) bool {
	return modTime.IsZero() || now.Sub(modTime) >= CacheTTL
}

// Stats summarizes a catalog for reporting.
type Stats struct {
	Providers, Models, Priced int
}

// Stats counts supported providers, picker-eligible models, and priced model IDs.
func (c Catalog) Stats() Stats {
	return Stats{Providers: len(c), Models: len(c.modelInfos()), Priced: len(c.prices())}
}

// Apply makes the catalog active process-wide: prices become the budget
// catalog layer, context windows feed model.KnownContextLimit, and models
// join the /model picker. A nil catalog removes all three layers.
func Apply(c Catalog) {
	if c == nil {
		budget.SetCatalogPrices(nil)
		model.SetContextLimitResolver(nil)
		modelinfo.SetCatalogModels(nil)
		return
	}
	budget.SetCatalogPrices(c.prices())
	limits := c.contextLimits()
	model.SetContextLimitResolver(func(name string) (int, bool) {
		for _, key := range modelid.Candidates(name) {
			if limit, ok := limits[key]; ok {
				return limit, true
			}
		}
		return 0, false
	})
	modelinfo.SetCatalogModels(c.modelInfos())
}

// eachModel visits supported providers in precedence order and their models
// in ID order, so first-wins merges are deterministic.
func (c Catalog) eachModel(visit func(chronosProvider, catalogProvider string, m Model)) {
	for _, mapping := range providerMapping {
		provider, ok := c[mapping.catalog]
		if !ok {
			continue
		}
		ids := make([]string, 0, len(provider.Models))
		for id := range provider.Models {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			m := provider.Models[id]
			if m.ID == "" {
				m.ID = id
			}
			visit(mapping.chronos, mapping.catalog, m)
		}
	}
}

// prices converts catalog costs to budget prices keyed by lowercase model ID.
// Models without a positive input and output price are left unpriced.
func (c Catalog) prices() map[string]budget.ModelPrice {
	prices := make(map[string]budget.ModelPrice)
	c.eachModel(func(_, catalogProvider string, m Model) {
		key := strings.ToLower(m.ID)
		if _, taken := prices[key]; taken || m.Cost == nil {
			return
		}
		price, err := m.Cost.modelPrice(catalogProvider)
		if err != nil {
			return
		}
		prices[key] = price
	})
	return prices
}

func (cost Cost) modelPrice(catalogProvider string) (budget.ModelPrice, error) {
	base := cost.Rates.usd(catalogProvider)
	var tiers []budget.USDTier
	for _, tier := range cost.Tiers {
		if tier.Tier.Type == "context" && tier.Tier.Size > 0 {
			tiers = append(tiers, budget.USDTier{AbovePromptTokens: int(tier.Tier.Size), USDRates: tier.Rates.usd(catalogProvider)})
		}
	}
	if len(tiers) == 0 && cost.ContextOver200K != nil {
		tiers = append(tiers, budget.USDTier{AbovePromptTokens: 200_000, USDRates: cost.ContextOver200K.usd(catalogProvider)})
	}
	slices.SortFunc(tiers, func(a, b budget.USDTier) int { return cmp.Compare(a.AbovePromptTokens, b.AbovePromptTokens) })
	tiers = slices.CompactFunc(tiers, func(a, b budget.USDTier) bool { return a.AbovePromptTokens == b.AbovePromptTokens })
	return budget.NewModelPrice(base, tiers...)
}

// usd converts catalog rates. models.dev does not publish Anthropic's 1-hour
// cache-write rate; Anthropic bills it at 2x base input.
func (r Rates) usd(catalogProvider string) budget.USDRates {
	rates := budget.USDRates{Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite}
	if catalogProvider == "anthropic" {
		oneHour := 2 * r.Input
		rates.CacheWrite1h = &oneHour
	}
	return rates
}

// contextLimits maps lowercase model IDs to the usable prompt window: the
// input limit when models.dev publishes a smaller one, else the context
// window.
func (c Catalog) contextLimits() map[string]int {
	limits := make(map[string]int)
	c.eachModel(func(_, _ string, m Model) {
		key := strings.ToLower(m.ID)
		if _, taken := limits[key]; taken {
			return
		}
		limit := m.Limit.Context
		if m.Limit.Input > 0 && m.Limit.Input < limit {
			limit = m.Limit.Input
		}
		if limit >= 1 && limit <= math.MaxInt32 {
			limits[key] = int(limit)
		}
	})
	return limits
}

// modelInfos returns picker entries: tool-calling, non-deprecated models,
// since an agent cannot drive a model without tool calls.
func (c Catalog) modelInfos() []modelinfo.Info {
	var infos []modelinfo.Info
	c.eachModel(func(chronosProvider, _ string, m Model) {
		if m.ToolCall && m.Status != "deprecated" {
			infos = append(infos, modelinfo.Info{Provider: chronosProvider, Model: m.ID})
		}
	})
	return infos
}
