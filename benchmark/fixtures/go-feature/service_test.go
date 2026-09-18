package records

import "testing"

func TestLookup(t *testing.T) {
	store := NewStore()
	store.Put("a", "one")
	service := NewService(store)
	if got, err := service.Lookup("a"); err != nil || got != "one" {
		t.Fatalf("Lookup() = %q, %v", got, err)
	}
}
