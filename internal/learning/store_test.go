package learning

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestStore_SaveListGet(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	sug := &Suggestion{ID: "learn_1", Kind: "agent", AgentID: "foo", Title: "t", Confidence: 0.5, YAML: "id: foo\n", CreatedAt: time.Now()}
	if err := store.Save(sug); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get("learn_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Title != "t" || got.AgentID != "foo" {
		t.Errorf("Get() = %+v, want title=t agent_id=foo", got)
	}

	all, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 1 || all[0].ID != "learn_1" {
		t.Errorf("List() = %+v, want one suggestion learn_1", all)
	}
}

func TestStore_List_EmptyWhenNoPendingDir(t *testing.T) {
	store := NewStore(t.TempDir())
	all, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 0 {
		t.Errorf("List() = %+v, want empty", all)
	}
}

func TestStore_Reject(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sug := &Suggestion{ID: "learn_reject", Kind: "pattern", Title: "t"}
	if err := store.Save(sug); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := store.Reject("learn_reject"); err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	if _, err := store.Get("learn_reject"); err == nil {
		t.Error("Get() after Reject: want error, got nil")
	}
}

func TestStore_Accept_AgentKindWritesAgentYAML(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	agentYAML := "id: search-first\nname: Search-First Agent\n"
	sug := &Suggestion{ID: "learn_agent", Kind: "agent", AgentID: "search-first", YAML: agentYAML}
	if err := store.Save(sug); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := store.Accept("learn_agent"); err != nil {
		t.Fatalf("Accept() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "agents", "search-first.yaml"))
	if err != nil {
		t.Fatalf("read accepted agent file: %v", err)
	}
	if string(data) != agentYAML {
		t.Errorf("accepted agent file = %q, want %q", data, agentYAML)
	}

	if _, err := store.Get("learn_agent"); err == nil {
		t.Error("pending suggestion still present after Accept()")
	}
}

func TestStore_Accept_PatternKindAppendsToPatternsFile(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sug := &Suggestion{ID: "learn_pattern", Kind: "pattern", Title: "Always grep before read", YAML: "grep before read saves a turn"}
	if err := store.Save(sug); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := store.Accept("learn_pattern"); err != nil {
		t.Fatalf("Accept() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "patterns.yaml"))
	if err != nil {
		t.Fatalf("read patterns.yaml: %v", err)
	}
	var doc patternsDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal patterns.yaml: %v", err)
	}
	if len(doc.Patterns) != 1 || doc.Patterns[0].Title != "Always grep before read" {
		t.Errorf("patterns.yaml = %+v, want one pattern titled 'Always grep before read'", doc.Patterns)
	}
}

func TestStore_Accept_UnknownID(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Accept("does-not-exist"); err == nil {
		t.Error("Accept() with unknown id: want error, got nil")
	}
}
