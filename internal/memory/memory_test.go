package memory

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos/storage"
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
			if !rec.Validated || rec.Repository == "" || rec.Source == "" || rec.Revision == "" || rec.Fingerprint == "" {
				t.Fatalf("Add did not create provenance-bearing record: %+v", rec)
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

func TestForContextPartitionsTenantsAndPreservesDefault(t *testing.T) {
	s := NewStore(t.TempDir())
	defaultStore := s.ForContext(context.Background())
	if defaultStore != s {
		t.Fatal("default tenant did not preserve the local store")
	}

	tenantA := s.ForContext(storage.WithTenant(context.Background(), "tenant-a"))
	tenantB := s.ForContext(storage.WithTenant(context.Background(), "tenant-b"))
	if _, err := tenantA.Add(CategoryProject, "tenant A only"); err != nil {
		t.Fatalf("tenant A Add: %v", err)
	}
	if _, err := tenantB.Add(CategoryProject, "tenant B only"); err != nil {
		t.Fatalf("tenant B Add: %v", err)
	}

	for _, tt := range []struct {
		name    string
		store   *Store
		content string
	}{
		{name: "default", store: defaultStore},
		{name: "tenant-a", store: tenantA, content: "tenant A only"},
		{name: "tenant-b", store: tenantB, content: "tenant B only"},
	} {
		records, err := tt.store.List(CategoryProject)
		if err != nil {
			t.Fatalf("%s List: %v", tt.name, err)
		}
		if tt.content == "" && len(records) != 0 {
			t.Fatalf("default records = %+v, want empty", records)
		}
		if tt.content != "" && (len(records) != 1 || records[0].Content != tt.content) {
			t.Fatalf("%s records = %+v, want only its memory", tt.name, records)
		}
	}

	if strings.Contains(tenantA.dir, "tenant-a") || strings.Contains(tenantB.dir, "tenant-b") {
		t.Fatalf("tenant directories expose raw tenant IDs: %q, %q", tenantA.dir, tenantB.dir)
	}
}

func TestTenantCannotListRecallOrDeleteAnotherTenantMemory(t *testing.T) {
	store := NewStore(t.TempDir())
	tenantA := store.ForContext(storage.WithTenant(context.Background(), "tenant-a"))
	tenantB := store.ForContext(storage.WithTenant(context.Background(), "tenant-b"))
	now := time.Now()
	const sharedID = "mem_shared"

	if err := tenantA.save(CategoryProject, fileDoc{Records: []Record{{
		ID: sharedID, Category: CategoryProject, Content: "shared query tenant A", Validated: true, CreatedAt: now,
	}}}); err != nil {
		t.Fatalf("seed tenant A: %v", err)
	}
	if err := tenantB.save(CategoryProject, fileDoc{Records: []Record{{
		ID: sharedID, Category: CategoryProject, Content: "shared query tenant B", Validated: true, CreatedAt: now,
	}}}); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	records, err := tenantA.List("")
	if err != nil {
		t.Fatalf("tenant A List: %v", err)
	}
	if len(records) != 1 || records[0].Content != "shared query tenant A" {
		t.Fatalf("tenant A List = %+v, want only tenant A memory", records)
	}

	recalled, err := tenantA.Recall("shared query", 10)
	if err != nil {
		t.Fatalf("tenant A Recall: %v", err)
	}
	if len(recalled) != 1 || recalled[0].Record.Content != "shared query tenant A" {
		t.Fatalf("tenant A Recall = %+v, want only tenant A memory", recalled)
	}

	if err := tenantA.Forget(sharedID); err != nil {
		t.Fatalf("tenant A Forget: %v", err)
	}
	records, err = tenantB.List("")
	if err != nil {
		t.Fatalf("tenant B List after tenant A delete: %v", err)
	}
	if len(records) != 1 || records[0].Content != "shared query tenant B" {
		t.Fatalf("tenant B records after tenant A delete = %+v, want tenant B memory intact", records)
	}
}

func TestRecallExcludesUnvalidatedAndInvalidatedRecords(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	if err := s.save(CategoryProject, fileDoc{Records: []Record{
		{ID: "valid", Category: CategoryProject, Content: "shared valid fact", Validated: true, CreatedAt: now},
		{ID: "unvalidated", Category: CategoryProject, Content: "shared unchecked fact", CreatedAt: now},
		{ID: "invalidated", Category: CategoryProject, Content: "shared stale fact", Validated: true, Invalidated: true, CreatedAt: now},
	}}); err != nil {
		t.Fatalf("save records: %v", err)
	}

	for _, query := range []string{"shared", ""} {
		got, err := s.Recall(query, 10)
		if err != nil {
			t.Fatalf("Recall(%q): %v", query, err)
		}
		if len(got) != 1 || got[0].Record.ID != "valid" {
			t.Fatalf("Recall(%q) = %+v, want only validated record", query, got)
		}
	}
}

func TestListMigratesLegacyRecordWithoutContentLoss(t *testing.T) {
	s := NewStore(t.TempDir())
	legacy := "records:\n  - id: mem_legacy\n    category: project\n    content: keep this content\n    created_at: 2026-01-02T03:04:05Z\n"
	if err := os.WriteFile(s.pathFor(CategoryProject), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy record: %v", err)
	}

	records, err := s.List(CategoryProject)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].ID != "mem_legacy" || records[0].Content != "keep this content" || records[0].Kind != "fact" {
		t.Fatalf("legacy record was not migrated losslessly: %+v", records[0])
	}
	if records[0].Validated {
		t.Fatalf("legacy record must remain unvalidated: %+v", records[0])
	}
}

func TestContextBlockExcludesUnvalidatedAndInvalidatedRecords(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	if err := s.save(CategoryProject, fileDoc{Records: []Record{
		{ID: "valid", Category: CategoryProject, Content: "current fact", Kind: "fact", Validated: true, CreatedAt: now},
		{ID: "unvalidated", Category: CategoryProject, Content: "unchecked fact", Kind: "fact", CreatedAt: now},
		{ID: "stale", Category: CategoryProject, Content: "stale fact", Kind: "fact", Validated: true, Invalidated: true, CreatedAt: now},
	}}); err != nil {
		t.Fatalf("save records: %v", err)
	}

	block, err := s.ContextBlock(10)
	if err != nil {
		t.Fatalf("ContextBlock: %v", err)
	}
	if !strings.Contains(block, "current fact") || strings.Contains(block, "unchecked fact") || strings.Contains(block, "stale fact") {
		t.Fatalf("ContextBlock included unusable records: %q", block)
	}
}

func TestContextBlockReturnsEmptyWhenNoUsableRecords(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.save(CategoryProject, fileDoc{Records: []Record{
		{ID: "unvalidated", Category: CategoryProject, Content: "unchecked", Kind: "fact", CreatedAt: time.Now()},
	}}); err != nil {
		t.Fatalf("save records: %v", err)
	}

	block, err := s.ContextBlock(10)
	if err != nil {
		t.Fatalf("ContextBlock: %v", err)
	}
	if block != "" {
		t.Fatalf("ContextBlock = %q, want empty", block)
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

func TestExtractFromMessageDoesNotDuplicateCommonIntentHandling(t *testing.T) {
	for _, message := range []string{"remember: use tabs", "Never commit directly to main", "What does this function do?"} {
		category, content, extracted := ExtractFromMessage(message)
		if extracted || category != "" || content != "" {
			t.Fatalf("ExtractFromMessage(%q) = (%q, %q, %t), want inert compatibility helper", message, category, content, extracted)
		}
	}
}
