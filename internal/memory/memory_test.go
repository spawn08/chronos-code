package memory

import (
	"fmt"
	"strings"
	"testing"
)

func TestAddListRoundTrip(t *testing.T) {
	for _, cat := range []Category{CategoryProject, CategoryUser, CategoryFeedback} {
		t.Run(string(cat), func(t *testing.T) {
			dir := t.TempDir()
			s := NewStore(dir)

			rec, err := s.Add(cat, "hello "+string(cat))
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			if rec.ID == "" {
				t.Fatalf("expected non-empty ID")
			}
			if rec.Category != cat {
				t.Fatalf("expected category %q, got %q", cat, rec.Category)
			}

			got, err := s.List(cat)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 record, got %d", len(got))
			}
			if got[0].ID != rec.ID || got[0].Content != rec.Content {
				t.Fatalf("round-trip mismatch: want %+v, got %+v", rec, got[0])
			}
		})
	}
}

func TestAddInvalidCategory(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.Add(Category("bogus"), "x"); err == nil {
		t.Fatalf("expected error for invalid category")
	}
}

func TestListAllCombinedAndSorted(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if _, err := s.Add(CategoryProject, "p1"); err != nil {
		t.Fatalf("Add project: %v", err)
	}
	if _, err := s.Add(CategoryUser, "u1"); err != nil {
		t.Fatalf("Add user: %v", err)
	}
	if _, err := s.Add(CategoryFeedback, "f1"); err != nil {
		t.Fatalf("Add feedback: %v", err)
	}

	all, err := s.List("")
	if err != nil {
		t.Fatalf("List(\"\"): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 records total, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.Before(all[i-1].CreatedAt) {
			t.Fatalf("records not sorted ascending by CreatedAt: %+v", all)
		}
	}
}

func TestListMissingFileIsEmptyNotError(t *testing.T) {
	s := NewStore(t.TempDir())
	got, err := s.List(CategoryProject)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list, got %d", len(got))
	}
}

func TestSearch(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if _, err := s.Add(CategoryProject, "Uses Makefile for builds"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.Add(CategoryUser, "Prefers ripgrep over grep"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	results, err := s.Search("MAKEFILE")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || !strings.Contains(results[0].Content, "Makefile") {
		t.Fatalf("expected 1 case-insensitive match, got %+v", results)
	}

	none, err := s.Search("nonexistent-term-xyz")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(none))
	}

	all, err := s.Search("")
	if err != nil {
		t.Fatalf("Search empty: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected empty query to match everything (2), got %d", len(all))
	}
}

func TestForget(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	rec, err := s.Add(CategoryFeedback, "never do X")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	other, err := s.Add(CategoryProject, "keep me")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := s.Forget(rec.ID); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	all, err := s.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 || all[0].ID != other.ID {
		t.Fatalf("expected only the other record to remain, got %+v", all)
	}

	if err := s.Forget("mem_doesnotexist"); err == nil {
		t.Fatalf("expected not-found error for unknown id")
	}
}

func TestContextBlockEmptyStore(t *testing.T) {
	s := NewStore(t.TempDir())
	block, err := s.ContextBlock(10)
	if err != nil {
		t.Fatalf("ContextBlock: %v", err)
	}
	if block != "" {
		t.Fatalf("expected empty block for empty store, got %q", block)
	}
}

func TestContextBlockNonEmpty(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if _, err := s.Add(CategoryProject, "Build with make build"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.Add(CategoryFeedback, "Always run go test before committing"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	block, err := s.ContextBlock(10)
	if err != nil {
		t.Fatalf("ContextBlock: %v", err)
	}
	if block == "" {
		t.Fatalf("expected non-empty block")
	}
	if !strings.Contains(block, "Known project/user/feedback notes:") {
		t.Fatalf("expected header in block, got %q", block)
	}
	if !strings.Contains(block, "[project]") || !strings.Contains(block, "[feedback]") {
		t.Fatalf("expected category tags in block, got %q", block)
	}
	if len(block) > contextBlockMaxTotalLen {
		t.Fatalf("block exceeds cap: len=%d", len(block))
	}
}

func TestContextBlockStressCapsLength(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	longContent := strings.Repeat("x", 500)
	for i := 0; i < 50; i++ {
		if _, err := s.Add(CategoryProject, fmt.Sprintf("%s-%d", longContent, i)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	block, err := s.ContextBlock(50)
	if err != nil {
		t.Fatalf("ContextBlock: %v", err)
	}
	if len(block) > contextBlockMaxTotalLen+200 {
		// Allow a little slack: cap check happens before appending a line,
		// so the block should never wildly exceed the budget.
		t.Fatalf("block did not respect length cap: len=%d", len(block))
	}
}

func TestExtractFromMessage(t *testing.T) {
	tests := []struct {
		name         string
		msg          string
		wantCategory Category
		wantExtract  bool
	}{
		{
			name:         "triggering: remember",
			msg:          "Please remember that I prefer tabs over spaces",
			wantCategory: CategoryFeedback,
			wantExtract:  true,
		},
		{
			name:         "triggering: never",
			msg:          "Never commit directly to main",
			wantCategory: CategoryFeedback,
			wantExtract:  true,
		},
		{
			name:         "non-triggering",
			msg:          "What does this function do?",
			wantCategory: "",
			wantExtract:  false,
		},
		{
			name:         "empty string",
			msg:          "",
			wantCategory: "",
			wantExtract:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat, content, extracted := ExtractFromMessage(tt.msg)
			if extracted != tt.wantExtract {
				t.Fatalf("extracted = %v, want %v", extracted, tt.wantExtract)
			}
			if cat != tt.wantCategory {
				t.Fatalf("category = %q, want %q", cat, tt.wantCategory)
			}
			if extracted && content != strings.TrimSpace(tt.msg) {
				t.Fatalf("content = %q, want %q", content, strings.TrimSpace(tt.msg))
			}
			if !extracted && content != "" {
				t.Fatalf("expected empty content when not extracted, got %q", content)
			}
		})
	}
}
