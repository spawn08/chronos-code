package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos/sdk/agent"
)

func TestLoadLearnedAgents(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "search-first.yaml"), []byte("id: search-first\nname: Search-First Agent\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// Malformed (no id) — must be skipped, not fail the whole load.
	if err := os.WriteFile(filepath.Join(dir, "malformed.yaml"), []byte("name: no id here\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// Non-YAML file — must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not yaml"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	agents, err := loadLearnedAgents(dir)
	if err != nil {
		t.Fatalf("loadLearnedAgents() error = %v", err)
	}
	if len(agents) != 1 || agents[0].ID != "search-first" {
		t.Errorf("loadLearnedAgents() = %+v, want one agent with id=search-first", agents)
	}
}

func TestLoadLearnedAgents_MissingDirIsNotError(t *testing.T) {
	agents, err := loadLearnedAgents(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("loadLearnedAgents() error = %v, want nil for a missing directory", err)
	}
	if agents != nil {
		t.Errorf("loadLearnedAgents() = %+v, want nil", agents)
	}
}

func TestMergeLearnedAgents_HandAuthoredWins(t *testing.T) {
	existing := []agent.AgentConfig{{ID: "coder", Name: "Hand-authored coder"}}
	learned := []agent.AgentConfig{
		{ID: "coder", Name: "Learned coder — must not override"},
		{ID: "search-first", Name: "Learned search-first"},
	}

	merged := mergeLearnedAgents(existing, learned)

	if len(merged) != 2 {
		t.Fatalf("mergeLearnedAgents() = %+v, want 2 agents", merged)
	}
	byID := make(map[string]agent.AgentConfig, len(merged))
	for _, a := range merged {
		byID[a.ID] = a
	}
	if byID["coder"].Name != "Hand-authored coder" {
		t.Errorf("coder.Name = %q, want hand-authored config to win", byID["coder"].Name)
	}
	if byID["search-first"].Name != "Learned search-first" {
		t.Errorf("search-first.Name = %q, want the learned agent to be appended", byID["search-first"].Name)
	}
}
