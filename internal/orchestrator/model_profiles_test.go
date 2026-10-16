package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"gopkg.in/yaml.v3"
)

type profileProvider struct {
	resourceProvider
	id string
}

func (p profileProvider) Model() string { return p.id }

func TestModelProfilesUseRequestProviderAndPreservePins(t *testing.T) {
	a := pipelineAgent(t)
	a.Model = profileProvider{id: "claude-test"}
	a.ContextPinsFn = func(context.Context) []model.Message {
		return []model.Message{{Role: model.RoleSystem, Content: "existing policy"}}
	}
	o := &Orchestrator{cfg: &config.Config{Tools: config.ToolsConfig{ModelProfiles: true}}}
	if err := o.installModelProfiles(a); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, want, absent string }{{"claude-sonnet-test", "thinking blocks", "Omit optional"}, {"gpt-test", "Omit optional", "thinking blocks"}, {"unmatched", "Use only exposed", "thinking blocks"}} {
		pins := a.ContextPinsFn(agent.WithModelProvider(context.Background(), profileProvider{id: tc.id}))
		if len(pins) != 2 || pins[0].Content != "existing policy" || !strings.Contains(pins[1].Content, tc.want) || strings.Contains(pins[1].Content, tc.absent) {
			t.Fatalf("%s pins: %v", tc.id, pins)
		}
	}
	pins := a.ContextPinsFn(context.Background())
	if !strings.Contains(pins[1].Content, "thinking blocks") {
		t.Fatalf("configured provider ignored: %v", pins)
	}
}

func TestModelProfilesValidateYAMLAndFirstMatch(t *testing.T) {
	for _, data := range []string{"profiles: [{model: '[', guidance: test}]", "profiles: [{model: 'gpt-*'}]", "profiles: [{model: 'gpt-*', guidance: a}, {model: 'gpt-*', guidance: b}]", "profiles: [{model: '*', guidance: a, unknown: b}]"} {
		if _, err := parseModelProfiles([]byte(data)); err == nil {
			t.Fatalf("invalid profiles accepted: %s", data)
		}
	}
	profiles, err := parseModelProfiles([]byte("shared_guidance: shared\nprofiles: [{model: 'gpt-*', guidance: first}, {model: '*', guidance: second}]"))
	if err != nil {
		t.Fatal(err)
	}
	if got := profiles.guidance("gpt-test"); got != "shared\nfirst" {
		t.Fatalf("overlapping profile: %q", got)
	}
}

func TestMainPromptDefersCanonicalNavigationAndKeepsDeliveryPolicy(t *testing.T) {
	data, err := defaults.ReadFile("agents/chronos-code.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var main struct {
		SystemPrompt string `yaml:"system_prompt"`
	}
	if err := yaml.Unmarshal(data, &main); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"make the change", "pinned intent/evidence and code-navigation policy", "After editing code", "Never revert changes you did not make", "STATUS (complete/partial/blocked/failed)"} {
		if !strings.Contains(main.SystemPrompt, required) {
			t.Fatalf("delivery policy lost %q", required)
		}
	}
	if strings.Contains(main.SystemPrompt, "codebase_search, resolve_symbol, codebase_context") {
		t.Fatal("main prompt duplicates canonical navigation catalog")
	}
	profiles, err := defaults.ReadFile("model-profiles.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseModelProfiles(profiles); err != nil {
		t.Fatal(err)
	}
}
