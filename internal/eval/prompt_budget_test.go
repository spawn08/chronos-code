package eval

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/model"
	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos-code/internal/defaults"
)

const (
	basePromptBudget  = 800
	totalPromptBudget = 1200

	// Estimated token overhead for skills block + memory context that gets
	// appended to the system prompt at runtime. The actual size varies per
	// turn; this is the planning ceiling from PRD P1-005.
	skillsMemoryOverhead = 400
)

type agentYAML struct {
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	SystemPrompt string `yaml:"system_prompt"`
}

func TestPromptTokenBudget(t *testing.T) {
	counter := model.NewTokenCounter("claude-sonnet-4-6")

	entries, err := fs.ReadDir(defaults.FS, "agents")
	if err != nil {
		t.Fatalf("read embedded agents dir: %v", err)
	}

	if len(entries) == 0 {
		t.Fatal("no agent YAML files found in embedded defaults")
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "\n%-15s %8s %8s %s\n", "AGENT", "BASE", "TOTAL", "STATUS")
	fmt.Fprintf(&summary, "%s\n", strings.Repeat("-", 50))

	allPassed := true
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		data, err := defaults.ReadFile("agents/" + entry.Name())
		if err != nil {
			t.Errorf("read %s: %v", entry.Name(), err)
			continue
		}
		var ag agentYAML
		if err := yaml.Unmarshal(data, &ag); err != nil {
			t.Errorf("parse %s: %v", entry.Name(), err)
			continue
		}
		if ag.SystemPrompt == "" {
			t.Errorf("agent %s (%s) has no system_prompt", ag.ID, entry.Name())
			continue
		}

		baseTokens := counter.CountString(ag.SystemPrompt)
		totalTokens := baseTokens + skillsMemoryOverhead

		status := "OK"
		if baseTokens > basePromptBudget {
			status = fmt.Sprintf("OVER BASE (%d > %d)", baseTokens, basePromptBudget)
			allPassed = false
		}
		if totalTokens > totalPromptBudget {
			status = fmt.Sprintf("OVER TOTAL (%d > %d)", totalTokens, totalPromptBudget)
			allPassed = false
		}

		fmt.Fprintf(&summary, "%-15s %8d %8d %s\n", ag.ID, baseTokens, totalTokens, status)

		t.Run(ag.ID+"/base", func(t *testing.T) {
			if baseTokens > basePromptBudget {
				t.Errorf("system prompt is %d tokens, budget is %d", baseTokens, basePromptBudget)
			}
		})
		t.Run(ag.ID+"/total", func(t *testing.T) {
			if totalTokens > totalPromptBudget {
				t.Errorf("system prompt (%d) + overhead (%d) = %d tokens, budget is %d",
					baseTokens, skillsMemoryOverhead, totalTokens, totalPromptBudget)
			}
		})
	}

	t.Log(summary.String())

	if !allPassed {
		t.Log("To fix: shorten the system_prompt in the agent's YAML under internal/defaults/agents/")
	}
}
