// Package modelinfo is a small, best-effort registry of known model IDs to
// their provider and context window size, used by the TUI's /model picker
// (ROADMAP.md §5.10 "visibility of models ... based on context window") so
// a user can see what they're switching to before committing. Entries here
// are a convenience, not an authoritative source — a model missing from
// this table still works (agent.BuildProvider doesn't consult it at all),
// it just shows as "context window: unknown" in the picker.
package modelinfo

import "sort"

// Info describes one known model.
type Info struct {
	Provider      string
	Model         string
	ContextWindow int // tokens; callers should treat 0 as "unknown", not "zero"
}

// registry is intentionally small and will go stale as new models ship —
// extend it as needed rather than treating it as complete. Context window
// figures are the vendor-documented maximums as of this package's last
// update, not necessarily what every account tier/endpoint grants.
var registry = []Info{
	{Provider: "anthropic", Model: "claude-opus-4-7", ContextWindow: 200_000},
	{Provider: "anthropic", Model: "claude-opus-4-8", ContextWindow: 200_000},
	{Provider: "anthropic", Model: "claude-sonnet-4-6", ContextWindow: 200_000},
	{Provider: "anthropic", Model: "claude-sonnet-4-5", ContextWindow: 200_000},
	{Provider: "anthropic", Model: "claude-haiku-4-5", ContextWindow: 200_000},

	{Provider: "openai", Model: "gpt-5", ContextWindow: 400_000},
	{Provider: "openai", Model: "gpt-5-mini", ContextWindow: 400_000},
	{Provider: "openai", Model: "gpt-5-nano", ContextWindow: 400_000},
	{Provider: "openai", Model: "gpt-4o", ContextWindow: 128_000},
	{Provider: "openai", Model: "gpt-4o-mini", ContextWindow: 128_000},
	{Provider: "openai", Model: "o3", ContextWindow: 200_000},

	{Provider: "gemini", Model: "gemini-2.5-pro", ContextWindow: 1_000_000},
	{Provider: "gemini", Model: "gemini-2.5-flash", ContextWindow: 1_000_000},
	{Provider: "gemini", Model: "gemini-1.5-pro", ContextWindow: 2_000_000},

	{Provider: "mistral", Model: "mistral-large-latest", ContextWindow: 128_000},
	{Provider: "deepseek", Model: "deepseek-chat", ContextWindow: 64_000},
	{Provider: "groq", Model: "llama-3.3-70b-versatile", ContextWindow: 128_000},
	{Provider: "openrouter", Model: "meta-llama/llama-3.1-405b", ContextWindow: 128_000},
}

// Lookup returns the registered Info for (provider, model), if known.
func Lookup(provider, model string) (Info, bool) {
	for _, i := range registry {
		if i.Provider == provider && i.Model == model {
			return i, true
		}
	}
	return Info{}, false
}

// LookupByModel finds a registered Info by model ID alone, for the common
// case where a user types just a model name (e.g. "/model gpt-4o") without
// specifying its provider. Returns ok=false if the model ID is unknown or
// ambiguous (registered under more than one provider).
func LookupByModel(modelID string) (Info, bool) {
	var found Info
	matches := 0
	for _, i := range registry {
		if i.Model == modelID {
			found = i
			matches++
		}
	}
	return found, matches == 1
}

// All returns every registered Info, grouped by provider then sorted by
// model name within each provider, for a stable /model listing.
func All() []Info {
	out := append([]Info(nil), registry...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}
