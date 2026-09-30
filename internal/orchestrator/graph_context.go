package orchestrator

import (
	"context"
	"fmt"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos-code/internal/defaults"
)

// installIndexGuidance couples navigation policy to runtime tool availability,
// including agents with custom or previously exported system prompts.
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
		previous := a.ContextPinsFn
		a.ContextPinsFn = func(ctx context.Context) []model.Message {
			var pins []model.Message
			if previous != nil {
				pins = append(pins, previous(ctx)...)
			}
			return append(pins, model.Message{Role: model.RoleSystem, Content: policy.SystemPrompt})
		}
	}
	return nil
}
