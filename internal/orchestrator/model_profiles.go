package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"gopkg.in/yaml.v3"
)

type modelProfileConfig struct {
	SharedGuidance string         `yaml:"shared_guidance"`
	Profiles       []modelProfile `yaml:"profiles"`
}

type modelProfile struct {
	Model    string `yaml:"model"`
	Guidance string `yaml:"guidance"`
}

func parseModelProfiles(data []byte) (modelProfileConfig, error) {
	var profiles modelProfileConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&profiles); err != nil {
		return profiles, fmt.Errorf("parse model profiles: %w", err)
	}
	seen := map[string]bool{}
	for _, profile := range profiles.Profiles {
		if profile.Model == "" || strings.TrimSpace(profile.Guidance) == "" {
			return profiles, fmt.Errorf("model profiles require model and guidance")
		}
		if _, err := path.Match(profile.Model, ""); err != nil {
			return profiles, fmt.Errorf("invalid model profile pattern %q: %w", profile.Model, err)
		}
		if seen[profile.Model] {
			return profiles, fmt.Errorf("duplicate model profile %q", profile.Model)
		}
		seen[profile.Model] = true
	}
	return profiles, nil
}

func (c modelProfileConfig) guidance(modelID string) string {
	guidance := strings.TrimSpace(c.SharedGuidance)
	for _, profile := range c.Profiles {
		if matches, _ := path.Match(profile.Model, modelID); matches {
			if guidance != "" {
				guidance += "\n"
			}
			return guidance + strings.TrimSpace(profile.Guidance)
		}
	}
	return guidance
}

func (o *Orchestrator) installModelProfiles(a *agent.Agent) error {
	if o.cfg == nil || !o.cfg.Tools.ModelProfiles {
		return nil
	}
	data, err := defaults.ReadFile("model-profiles.yaml")
	if err != nil {
		return fmt.Errorf("read model profiles: %w", err)
	}
	profiles, err := parseModelProfiles(data)
	if err != nil {
		return err
	}
	prev := a.ContextPinsFn
	a.ContextPinsFn = func(ctx context.Context) []model.Message {
		var pins []model.Message
		if prev != nil {
			pins = append(pins, prev(ctx)...)
		}
		provider := requestModelProvider(ctx, a)
		modelID := ""
		if provider != nil {
			modelID = provider.Model()
		}
		if guidance := profiles.guidance(modelID); guidance != "" {
			pins = append(pins, model.Message{Role: model.RoleSystem, Name: "model-tool-guidance", Content: guidance})
		}
		return pins
	}
	return nil
}
