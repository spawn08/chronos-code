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
	info, ok := LookupByModel("gpt-4o")
	if !ok || info.Provider != "openai" {
		t.Fatalf("LookupByModel(gpt-4o) = %+v, ok=%v, want openai match", info, ok)
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
