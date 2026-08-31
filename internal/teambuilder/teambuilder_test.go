package teambuilder

import (
	"testing"

	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/team"
)

func TestParseStrategy(t *testing.T) {
	tests := []struct {
		input string
		want  team.Strategy
		err   bool
	}{
		{"sequential", team.StrategySequential, false},
		{"parallel", team.StrategyParallel, false},
		{"router", team.StrategyRouter, false},
		{"coordinator", team.StrategyCoordinator, false},
		{"swarm", team.StrategySwarm, false},
		{"hierarchy", team.StrategyHierarchy, false},
		{"Sequential", team.StrategySequential, false},
		{"unknown", "", true},
	}
	for _, tt := range tests {
		got, err := parseStrategy(tt.input)
		if tt.err && err == nil {
			t.Errorf("parseStrategy(%q) expected error", tt.input)
		}
		if !tt.err && err != nil {
			t.Errorf("parseStrategy(%q) unexpected error: %v", tt.input, err)
		}
		if got != tt.want {
			t.Errorf("parseStrategy(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseErrorStrategy(t *testing.T) {
	tests := []struct {
		input string
		want  team.ErrorStrategy
		err   bool
	}{
		{"fail_fast", team.ErrorStrategyFailFast, false},
		{"failfast", team.ErrorStrategyFailFast, false},
		{"collect", team.ErrorStrategyCollect, false},
		{"best_effort", team.ErrorStrategyBestEffort, false},
		{"besteffort", team.ErrorStrategyBestEffort, false},
		{"unknown", 0, true},
	}
	for _, tt := range tests {
		got, err := parseErrorStrategy(tt.input)
		if tt.err && err == nil {
			t.Errorf("parseErrorStrategy(%q) expected error", tt.input)
		}
		if !tt.err && err != nil {
			t.Errorf("parseErrorStrategy(%q) unexpected error: %v", tt.input, err)
		}
		if got != tt.want {
			t.Errorf("parseErrorStrategy(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestBuild_UnknownAgent(t *testing.T) {
	tc := &agent.TeamConfig{
		ID:       "test-team",
		Strategy: "sequential",
		Agents:   []string{"nonexistent"},
	}
	_, err := Build(tc, map[string]*agent.Agent{})
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
}

func TestBuild_UnknownStrategy(t *testing.T) {
	tc := &agent.TeamConfig{
		ID:       "test-team",
		Strategy: "invalid",
		Agents:   []string{},
	}
	_, err := Build(tc, map[string]*agent.Agent{})
	if err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

func TestBuildAll_Empty(t *testing.T) {
	teams, err := BuildAll(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(teams) != 0 {
		t.Fatalf("expected 0 teams, got %d", len(teams))
	}
}
