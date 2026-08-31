package memory

import (
	"testing"
)

func seedStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(t.TempDir())
	records := []struct {
		cat     Category
		content string
	}{
		{CategoryProject, "Uses Makefile for builds, run make build to compile"},
		{CategoryProject, "Go project using module github.com/spawn08/chronos-code"},
		{CategoryUser, "Prefers ripgrep over grep for searching"},
		{CategoryFeedback, "Never commit directly to main branch"},
		{CategoryFeedback, "Always run go test before committing changes"},
	}
	for _, r := range records {
		if _, err := s.Add(r.cat, r.content); err != nil {
			t.Fatalf("seed Add: %v", err)
		}
	}
	return s
}

func TestRecall_RelevantRanksHigher(t *testing.T) {
	s := seedStore(t)
	results, err := s.Recall("Makefile build", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for 'Makefile build'")
	}
	if results[0].Record.Content != "Uses Makefile for builds, run make build to compile" {
		t.Fatalf("top result should be the Makefile record, got %q", results[0].Record.Content)
	}
	if results[0].Score <= 0 {
		t.Fatalf("expected positive score, got %f", results[0].Score)
	}
}

func TestRecall_ScoreOrdering(t *testing.T) {
	s := seedStore(t)
	results, err := s.Recall("go test", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Fatalf("results not sorted descending: [%d].Score=%f > [%d].Score=%f",
				i, results[i].Score, i-1, results[i-1].Score)
		}
	}
}

func TestRecall_PhraseMatchBoost(t *testing.T) {
	s := NewStore(t.TempDir())
	s.Add(CategoryProject, "run go test before deploy")
	s.Add(CategoryProject, "go modules and test frameworks are great")

	results, err := s.Recall("go test", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}
	// The record with the exact phrase "go test" should score higher.
	if results[0].Score <= results[1].Score {
		t.Fatalf("phrase match should rank higher: %f <= %f", results[0].Score, results[1].Score)
	}
}

func TestRecall_MaxResults(t *testing.T) {
	s := seedStore(t)
	results, err := s.Recall("go", 2)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) > 2 {
		t.Fatalf("expected at most 2 results, got %d", len(results))
	}
}

func TestRecall_EmptyQuery(t *testing.T) {
	s := seedStore(t)
	results, err := s.Recall("", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("empty query should return all 5 records, got %d", len(results))
	}
	for _, r := range results {
		if r.Score != 1.0 {
			t.Fatalf("empty query records should all score 1.0, got %f", r.Score)
		}
	}
}

func TestRecall_EmptyStore(t *testing.T) {
	s := NewStore(t.TempDir())
	results, err := s.Recall("anything", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil for empty store, got %v", results)
	}
}

func TestRecall_NoMatches(t *testing.T) {
	s := seedStore(t)
	results, err := s.Recall("xyzzy plugh frobnicate", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for gibberish query, got %d", len(results))
	}
}

func TestRecall_CategoryBoost(t *testing.T) {
	s := NewStore(t.TempDir())
	s.Add(CategoryProject, "build configuration details")
	s.Add(CategoryUser, "build configuration details")

	results, err := s.Recall("project build configuration", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Record.Category != CategoryProject {
		t.Fatalf("project record should rank first when query contains 'project', got %s",
			results[0].Record.Category)
	}
}

func TestRecall_UnlimitedMaxResults(t *testing.T) {
	s := seedStore(t)
	results, err := s.Recall("go", 0)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results with maxResults=0 (unlimited)")
	}
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"hello world", 2},
		{"a b c", 0},       // single-char tokens filtered
		{"foo-bar_baz", 3}, // punctuation splits
		{"", 0},
	}
	for _, tt := range tests {
		got := tokenize(tt.input)
		if len(got) != tt.want {
			t.Errorf("tokenize(%q) = %d tokens %v, want %d", tt.input, len(got), got, tt.want)
		}
	}
}
