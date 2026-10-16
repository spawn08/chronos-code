package orchestrator

import (
	"context"
	"fmt"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos-code/internal/defaults"
)

// installIndexGuidance pins intent, evidence, and navigation policy, including
// for agents with custom or previously exported system prompts. The policy
// includes direct-source fallback when the code index cannot be opened.
func installIndexGuidance(agents map[string]*agent.Agent) error {
	data, err := defaults.ReadFile("context-engineering.yaml")
	if err != nil {
		return fmt.Errorf("read index guidance: %w", err)
	}
	var policy struct {
		SystemPrompt string `yaml:"system_prompt"`
	}
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return fmt.Errorf("parse index guidance: %w", err)
	}
	for _, a := range agents {
		if a == nil {
			continue
		}
		previous := a.ContextPinsFn
		a.ContextPinsFn = func(ctx context.Context) []model.Message {
			var pins []model.Message
			if previous != nil {
				pins = append(pins, previous(ctx)...)
			}
			return append(pins, model.Message{Role: model.RoleSystem, Name: "harness-index-guidance", Content: policy.SystemPrompt})
		}
	}
	return nil
}
