package budget

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos-code/internal/modelid"
)

// PricingOverlay is one user- or project-supplied pricing.yaml document.
type PricingOverlay struct {
	Source string
	Data   []byte
}

// USDRates are USD per 1M tokens, matching provider price pages and the
// models.dev catalog. Nil cache rates take the pricing.yaml defaults: cache
// read and write default to Input, the 1-hour write rate to CacheWrite.
type USDRates struct {
	Input        float64
	Output       float64
	CacheRead    *float64
	CacheWrite   *float64
	CacheWrite1h *float64
}

// USDTier replaces the base rates for a whole call whose prompt exceeds
// AbovePromptTokens.
type USDTier struct {
	AbovePromptTokens int
	USDRates
}

type pricingFile struct {
	Models map[string]pricingEntry `yaml:"models"`
}

type pricingRates struct {
	Input        *float64 `yaml:"input"`
	Output       *float64 `yaml:"output"`
	CacheRead    *float64 `yaml:"cache_read"`
	CacheWrite   *float64 `yaml:"cache_write"`
	CacheWrite1h *float64 `yaml:"cache_write_1h"`
}

type pricingEntry struct {
	pricingRates `yaml:",inline"`
	Tiers        []pricingTier `yaml:"tiers"`
}

type pricingTier struct {
	Above        int `yaml:"above"`
	pricingRates `yaml:",inline"`
}

// maxUSDPerMillion bounds configured rates so token*rate stays far from
// int64 overflow for any realistic request size.
const maxUSDPerMillion = 10_000

var (
	priceTable     atomic.Pointer[map[string]ModelPrice]
	bundledOnce    sync.Once
	bundledPrices  map[string]ModelPrice
	bundledLoadErr error

	// layersMu guards the layers the active table is composed from, in
	// increasing precedence: bundled pricing.yaml, catalog, overlays.
	layersMu      sync.Mutex
	catalogPrices map[string]ModelPrice
	overlayTables []map[string]ModelPrice
)

// LoadPricing sets the user/project overlays (later overlays win per model
// ID) and recomposes the active table over the bundled and catalog layers.
// On error the active table is left unchanged.
func LoadPricing(overlays ...PricingOverlay) error {
	if _, err := bundledPriceTable(); err != nil {
		return err
	}
	tables := make([]map[string]ModelPrice, 0, len(overlays))
	for _, overlay := range overlays {
		parsed, err := parsePricing(overlay.Data)
		if err != nil {
			return fmt.Errorf("parse %s pricing.yaml: %w", overlay.Source, err)
		}
		tables = append(tables, parsed)
	}
	layersMu.Lock()
	defer layersMu.Unlock()
	overlayTables = tables
	recomposeLocked()
	return nil
}

// SetCatalogPrices replaces the catalog layer (e.g. models.dev), which sits
// above bundled pricing.yaml and below user/project overlays. Keys must be
// lowercase model IDs; nil clears the layer.
func SetCatalogPrices(prices map[string]ModelPrice) {
	layersMu.Lock()
	defer layersMu.Unlock()
	catalogPrices = prices
	recomposeLocked()
}

func recomposeLocked() {
	base, _ := bundledPriceTable()
	table := make(map[string]ModelPrice, len(base)+len(catalogPrices))
	for _, layer := range append([]map[string]ModelPrice{base, catalogPrices}, overlayTables...) {
		for id, price := range layer {
			table[id] = price
		}
	}
	priceTable.Store(&table)
}

func activePriceTable() map[string]ModelPrice {
	if table := priceTable.Load(); table != nil {
		return *table
	}
	base, err := bundledPriceTable()
	if err != nil {
		return nil
	}
	return base
}

func bundledPriceTable() (map[string]ModelPrice, error) {
	bundledOnce.Do(func() {
		data, err := defaults.ReadFile("pricing.yaml")
		if err != nil {
			bundledLoadErr = fmt.Errorf("read embedded pricing.yaml: %w", err)
			return
		}
		bundledPrices, bundledLoadErr = parsePricing(data)
		if bundledLoadErr != nil {
			bundledLoadErr = fmt.Errorf("parse embedded pricing.yaml: %w", bundledLoadErr)
		}
	})
	return bundledPrices, bundledLoadErr
}

func parsePricing(data []byte) (map[string]ModelPrice, error) {
	var file pricingFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	table := make(map[string]ModelPrice, len(file.Models))
	for id, entry := range file.Models {
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			return nil, fmt.Errorf("empty model ID")
		}
		price, err := entry.price()
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", id, err)
		}
		table[key] = price
	}
	return table, nil
}

func (e pricingEntry) price() (ModelPrice, error) {
	base, err := e.usd()
	if err != nil {
		return ModelPrice{}, err
	}
	tiers := make([]USDTier, 0, len(e.Tiers))
	for i, tier := range e.Tiers {
		rates, err := tier.usd()
		if err != nil {
			return ModelPrice{}, fmt.Errorf("tier %d: %w", i, err)
		}
		tiers = append(tiers, USDTier{AbovePromptTokens: tier.Above, USDRates: rates})
	}
	return NewModelPrice(base, tiers...)
}

func (r pricingRates) usd() (USDRates, error) {
	if r.Input == nil || r.Output == nil {
		return USDRates{}, fmt.Errorf("input and output rates are required")
	}
	return USDRates{Input: *r.Input, Output: *r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite, CacheWrite1h: r.CacheWrite1h}, nil
}

// NewModelPrice validates USD-per-1M rates and converts them. Tiers must have
// positive, strictly ascending thresholds.
func NewModelPrice(base USDRates, tiers ...USDTier) (ModelPrice, error) {
	rates, err := base.rates()
	if err != nil {
		return ModelPrice{}, err
	}
	price := ModelPrice{Rates: rates}
	for i, tier := range tiers {
		if tier.AbovePromptTokens <= 0 {
			return ModelPrice{}, fmt.Errorf("tier %d: above must be a positive prompt token count", i)
		}
		if i > 0 && tier.AbovePromptTokens <= tiers[i-1].AbovePromptTokens {
			return ModelPrice{}, fmt.Errorf("tier %d: tiers must be listed in ascending order of above", i)
		}
		tierRates, err := tier.rates()
		if err != nil {
			return ModelPrice{}, fmt.Errorf("tier %d: %w", i, err)
		}
		price.Tiers = append(price.Tiers, PriceTier{AbovePromptTokens: tier.AbovePromptTokens, Rates: tierRates})
	}
	return price, nil
}

func (r USDRates) rates() (Rates, error) {
	input, err := perMillionRate("input", r.Input)
	if err != nil {
		return Rates{}, err
	}
	output, err := perMillionRate("output", r.Output)
	if err != nil {
		return Rates{}, err
	}
	if input <= 0 || output <= 0 {
		return Rates{}, fmt.Errorf("input and output rates must be positive")
	}
	rates := Rates{InputPerMillion: input, OutputPerMillion: output, CacheReadPerMillion: input, CacheWritePerMillion: input}
	if r.CacheRead != nil {
		if rates.CacheReadPerMillion, err = perMillionRate("cache_read", *r.CacheRead); err != nil {
			return Rates{}, err
		}
	}
	if r.CacheWrite != nil {
		if rates.CacheWritePerMillion, err = perMillionRate("cache_write", *r.CacheWrite); err != nil {
			return Rates{}, err
		}
	}
	rates.CacheWrite1hPerMillion = rates.CacheWritePerMillion
	if r.CacheWrite1h != nil {
		if rates.CacheWrite1hPerMillion, err = perMillionRate("cache_write_1h", *r.CacheWrite1h); err != nil {
			return Rates{}, err
		}
	}
	return rates, nil
}

// perMillionRate converts USD per 1M tokens to integer microdollars per 1M
// tokens, which represents rates down to $0.000001/M exactly.
func perMillionRate(field string, usd float64) (Microdollars, error) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 || usd > maxUSDPerMillion {
		return 0, fmt.Errorf("%s rate %v must be between 0 and %d USD per 1M tokens", field, usd, maxUSDPerMillion)
	}
	return Microdollars(math.Round(usd * 1_000_000)), nil
}

// priceKeys returns lookup candidates for modelID, most specific first.
func priceKeys(modelID string) []string {
	return modelid.Candidates(modelID)
}
