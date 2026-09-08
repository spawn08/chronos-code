package modelinfo

import "testing"

func TestLookupKnownModel(t *testing.T) {
	info, ok := Lookup("anthropic", "claude-sonnet-4-6")
	if !ok {
		t.Fatal("Lookup: want found for a known model")
	}
	if info.ContextWindow != 200_000 {
		t.Errorf("ContextWindow = %d, want 200000", info.ContextWindow)
	}
}

func TestLookupUnknownModel(t *testing.T) {
	if _, ok := Lookup("anthropic", "not-a-real-model"); ok {
		t.Fatal("Lookup: want not found for an unregistered model")
	}
}

func TestLookupByModelUnambiguous(t *testing.T) {
	info, ok := LookupByModel("mistral-large-latest")
	if !ok || info.Provider != "mistral" {
		t.Fatalf("LookupByModel(mistral-large-latest) = %+v, ok=%v, want mistral match", info, ok)
	}
}

// gpt-4o is registered under both openai and azure (a deployment name is
// user-chosen, but conventionally mirrors the underlying model), so it's the
// ambiguous case LookupByModel's doc comment calls out — verify it actually
// resolves that way rather than silently picking one provider.
func TestLookupByModelAmbiguousAcrossProviders(t *testing.T) {
	if _, ok := LookupByModel("gpt-4o"); ok {
		t.Fatal("LookupByModel(gpt-4o): want false — registered under both openai and azure")
	}
}

func TestLookupByModelUnknownReturnsFalse(t *testing.T) {
	if _, ok := LookupByModel("totally-made-up"); ok {
		t.Fatal("LookupByModel: want false for an unregistered model")
	}
}

func TestAllIsSortedByProviderThenModel(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("All() returned nothing")
	}
	for i := 1; i < len(all); i++ {
		prev, cur := all[i-1], all[i]
		if prev.Provider > cur.Provider {
			t.Fatalf("not sorted by provider: %q before %q", prev.Provider, cur.Provider)
		}
		if prev.Provider == cur.Provider && prev.Model > cur.Model {
			t.Fatalf("not sorted by model within provider %q: %q before %q", prev.Provider, prev.Model, cur.Model)
		}
	}
}
