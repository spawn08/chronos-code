// Package modelinfo is a small, best-effort registry of known model IDs to
// their provider and context window size, used by the TUI's /model picker
// (ROADMAP.md §5.10 "visibility of models ... based on context window") so
// a user can see what they're switching to before committing. Entries here
// are a convenience, not an authoritative source — a model missing from
// this table still works (agent.BuildProvider doesn't consult it at all),
// it just shows as "context window: unknown" in the picker.
package modelinfo

import (
	"os"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/spawn08/chronos/engine/model"
)

// Info describes one known model.
type Info struct {
	Provider      string
	Model         string
	ContextWindow int // tokens; callers should treat 0 as "unknown", not "zero"
}

// registry is intentionally small and will go stale as new models ship —
// extend it as needed rather than treating it as complete. It records provider
// membership only; context windows are resolved from the SDK's known table.
var registry = []Info{
	{Provider: "anthropic", Model: "claude-fable-5"},
	{Provider: "anthropic", Model: "claude-opus-4-7"},
	{Provider: "anthropic", Model: "claude-opus-4-8"},
	{Provider: "anthropic", Model: "claude-sonnet-5"},
	{Provider: "anthropic", Model: "claude-sonnet-4-6"},
	{Provider: "anthropic", Model: "claude-sonnet-4-5"},
	{Provider: "anthropic", Model: "claude-haiku-4-5"},

	{Provider: "openai", Model: "gpt-5"},
	{Provider: "openai", Model: "gpt-5-mini"},
	{Provider: "openai", Model: "gpt-5-nano"},
	{Provider: "openai", Model: "gpt-4o"},
	{Provider: "openai", Model: "gpt-4o-mini"},
	{Provider: "openai", Model: "o3"},

	// gemini-2.5-* deliberately omitted: chronos's own compaction table
	// (modelContextLimits) has no entry for it, so configuring it would
	// silently fall back to an 8,192-token default context limit — these
	// are the Gemini model IDs that table actually recognizes.
	{Provider: "gemini", Model: "gemini-2.0-pro"},
	{Provider: "gemini", Model: "gemini-2.0-flash"},
	{Provider: "gemini", Model: "gemini-1.5-flash"},
	{Provider: "gemini", Model: "gemini-1.5-pro"},

	{Provider: "mistral", Model: "mistral-large-latest"},
	{Provider: "deepseek", Model: "deepseek-chat"},
	{Provider: "groq", Model: "llama-3.3-70b-versatile"},
	{Provider: "openrouter", Model: "meta-llama/llama-3.1-405b"},

	// Azure OpenAI: `model` here is the deployment name, which is
	// user-chosen and arbitrary — these are just the conventional
	// deployment names Azure's own quickstart docs use, so the /login and
	// /model pickers have at least one azure entry instead of none. Context
	// windows mirror the underlying OpenAI model above; pick whatever
	// deployment name matches your actual model if you named it
	// differently.
	{Provider: "azure", Model: "gpt-4o"},
	{Provider: "azure", Model: "gpt-4o-mini"},
	{Provider: "azure", Model: "gpt-5"},
}

func withContextWindow(info Info) Info {
	info.ContextWindow, _ = model.KnownContextLimit(info.Model)
	return info
}

// catalogModels holds models from an external catalog (models.dev), merged
// with the static registry by every lookup.
var catalogModels atomic.Pointer[[]Info]

// inferableProviders are the first-party providers whose catalog models may
// be matched by LookupByModel. Aggregators (openrouter, together, groq, ...)
// re-host the same model IDs, so including them would make common IDs
// ambiguous and break provider inference for a bare model name.
var inferableProviders = map[string]bool{"anthropic": true, "openai": true, "gemini": true, "mistral": true, "deepseek": true}

// SetCatalogModels replaces the catalog layer. Providers must use chronos
// canonical names (e.g. "gemini", not models.dev's "google"). nil clears it.
func SetCatalogModels(models []Info) {
	if models == nil {
		catalogModels.Store(nil)
		return
	}
	copied := append([]Info(nil), models...)
	catalogModels.Store(&copied)
}

func catalog() []Info {
	if models := catalogModels.Load(); models != nil {
		return *models
	}
	return nil
}

// Lookup returns the registered or catalog Info for (provider, model), if known.
func Lookup(provider, model string) (Info, bool) {
	provider = CanonicalProvider(provider)
	for _, source := range [][]Info{registry, catalog()} {
		for _, i := range source {
			if i.Provider == provider && i.Model == model {
				return withContextWindow(i), true
			}
		}
	}
	return Info{}, false
}

// LookupByModel finds a registered Info by model ID alone, for the common
// case where a user types just a model name (e.g. "/model gpt-4o") without
// specifying its provider. Returns ok=false if the model ID is unknown or
// ambiguous (registered under more than one provider). Catalog models count
// only for first-party providers (see inferableProviders).
func LookupByModel(modelID string) (Info, bool) {
	var found Info
	providers := make(map[string]bool)
	for _, i := range registry {
		if i.Model == modelID {
			found = i
			providers[i.Provider] = true
		}
	}
	for _, i := range catalog() {
		if i.Model == modelID && inferableProviders[i.Provider] && !providers[i.Provider] {
			found = i
			providers[i.Provider] = true
		}
	}
	return withContextWindow(found), len(providers) == 1
}

// All returns every registered Info, grouped by provider then sorted by
// model name within each provider, for a stable /model listing. If
// AZURE_OPENAI_DEPLOYMENT is set, that deployment name is included as an
// azure entry too — deployment names are the user's own choice, so the
// conventional examples in the registry above rarely match what's actually
// configured, and without this the /model picker would never show the
// deployment someone's AZURE_OPENAI_* env vars already point at.
func All() []Info {
	out := append([]Info(nil), registry...)
	seen := make(map[[2]string]bool, len(out))
	for _, i := range out {
		seen[[2]string{i.Provider, i.Model}] = true
	}
	for _, i := range catalog() {
		if key := [2]string{i.Provider, i.Model}; !seen[key] {
			seen[key] = true
			out = append(out, i)
		}
	}
	if dep := strings.TrimSpace(os.Getenv("AZURE_OPENAI_DEPLOYMENT")); dep != "" {
		if _, ok := Lookup("azure", dep); !ok {
			out = append(out, Info{Provider: "azure", Model: dep})
		}
	}
	for i := range out {
		out[i] = withContextWindow(out[i])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}
