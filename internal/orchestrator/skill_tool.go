package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/skills"
)

// skillToolName loads a skill's instructions on demand; the skill catalog
// pinned every turn tells the model which skills exist.
const skillToolName = "skill"

// skillTool returns the instructions of a skill a can run. Unavailable
// skills (missing required tools, model hint) are refused, as they are
// omitted from a's catalog.
func skillTool(a *agent.Agent, catalog []*skills.Skill) *tool.Definition {
	return &tool.Definition{
		Name:        skillToolName,
		Description: "Load the instructions of a skill listed in the skill catalog, by name. Follow the returned instructions for the task; files they mention are relative to base_dir when it is present.",
		Effects:     []tool.Effect{tool.EffectRead},
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Skill name from the skill catalog"},
			},
			"required": []string{"name"},
		},
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			name, _ := args["name"].(string)
			available := skills.Available(catalog, skillCapabilityManifest(a))
			selected := skills.Find(available, name)
			if selected == nil {
				names := make([]string, len(available))
				for i, s := range available {
					names[i] = s.Name
				}
				return nil, fmt.Errorf("skill: no available skill named %q; available: %s", name, strings.Join(names, ", "))
			}
			result := map[string]any{"name": selected.Name, "instructions": selected.Body}
			if selected.Source != "" && !strings.HasPrefix(selected.Source, "bundled:") {
				result["base_dir"] = filepath.Dir(selected.Source)
			}
			return result, nil
		},
	}
}
