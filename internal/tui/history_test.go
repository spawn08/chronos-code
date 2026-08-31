package tui

import "testing"

func TestHistory_PrevNext_RoundTrip(t *testing.T) {
	h := NewHistory()
	h.Add("first")
	h.Add("second")
	h.Add("third")

	v, ok := h.Prev("draft in progress")
	if !ok || v != "third" {
		t.Fatalf("Prev() = (%q, %v), want (%q, true)", v, ok, "third")
	}
	v, ok = h.Prev("")
	if !ok || v != "second" {
		t.Fatalf("Prev() = (%q, %v), want (%q, true)", v, ok, "second")
	}
	v, ok = h.Prev("")
	if !ok || v != "first" {
		t.Fatalf("Prev() = (%q, %v), want (%q, true)", v, ok, "first")
	}
	if _, ok := h.Prev(""); ok {
		t.Fatal("Prev() at oldest entry: ok = true, want false")
	}

	v, ok = h.Next()
	if !ok || v != "second" {
		t.Fatalf("Next() = (%q, %v), want (%q, true)", v, ok, "second")
	}
	v, ok = h.Next()
	if !ok || v != "third" {
		t.Fatalf("Next() = (%q, %v), want (%q, true)", v, ok, "third")
	}
	v, ok = h.Next()
	if !ok || v != "draft in progress" {
		t.Fatalf("Next() past newest = (%q, %v), want (%q, true) — draft restored", v, ok, "draft in progress")
	}
	if _, ok := h.Next(); ok {
		t.Fatal("Next() past draft: ok = true, want false")
	}
}

func TestHistory_EmptyHistory(t *testing.T) {
	h := NewHistory()
	if _, ok := h.Prev("x"); ok {
		t.Fatal("Prev() on empty history: ok = true, want false")
	}
	if _, ok := h.Next(); ok {
		t.Fatal("Next() on empty history: ok = true, want false")
	}
}

func TestHistory_Add_IgnoresBlankAndImmediateRepeat(t *testing.T) {
	h := NewHistory()
	h.Add("")
	h.Add("   ")
	h.Add("hello")
	h.Add("hello")
	h.Add("world")

	if len(h.entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 (blank/repeat ignored): %v", len(h.entries), h.entries)
	}
	v, ok := h.Prev("")
	if !ok || v != "world" {
		t.Fatalf("Prev() = (%q, %v), want (%q, true)", v, ok, "world")
	}
}

func TestHistory_Reset_EndsRecall(t *testing.T) {
	h := NewHistory()
	h.Add("one")
	h.Add("two")
	if _, ok := h.Prev("draft"); !ok {
		t.Fatal("Prev() ok = false, want true")
	}
	h.Reset()
	v, ok := h.Prev("new draft")
	if !ok || v != "two" {
		t.Fatalf("Prev() after Reset = (%q, %v), want (%q, true)", v, ok, "two")
	}
}

func TestHistory_Search(t *testing.T) {
	h := NewHistory()
	h.Add("find all callers of BuildAll")
	h.Add("explain the router")
	h.Add("find the config loader")

	if got := h.Search(""); got != nil {
		t.Fatalf("Search(\"\") = %v, want nil", got)
	}

	got := h.Search("find")
	want := []string{"find the config loader", "find all callers of BuildAll"}
	if len(got) != len(want) {
		t.Fatalf("Search(%q) = %v, want %v", "find", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Search(%q)[%d] = %q, want %q", "find", i, got[i], want[i])
		}
	}

	if got := h.Search("nonexistent"); got != nil {
		t.Fatalf("Search(%q) = %v, want nil", "nonexistent", got)
	}
}
