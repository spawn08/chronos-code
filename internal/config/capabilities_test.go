package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCapabilityManifestValidateAgainstDefaults(t *testing.T) {
	defaults := CapabilityManifest{Capabilities: []Capability{
		{Name: "file_read"},
		{Name: "shell"},
	}}
	manifest := CapabilityManifest{Capabilities: []Capability{
		{Name: "file_read"},
		{Name: "shell"},
	}}

	if err := manifest.Validate(defaults); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestCapabilityManifestValidateRejectsMissingRequiredCapability(t *testing.T) {
	manifest := CapabilityManifest{Capabilities: []Capability{{Name: "file_write"}}}
	err := manifest.Validate(CapabilityManifest{Capabilities: []Capability{{Name: "file_read"}}})
	if err == nil || !strings.Contains(err.Error(), `required capability "file_write"`) {
		t.Fatalf("Validate() error = %v, want missing required capability", err)
	}
}

func TestCapabilityManifestValidateAllowsMissingOptionalCapability(t *testing.T) {
	manifest := CapabilityManifest{Capabilities: []Capability{{Name: "lsp", Optional: true}}}
	if err := manifest.Validate(CapabilityManifest{}); err != nil {
		t.Fatalf("Validate() error = %v, want nil for optional capability", err)
	}
}

func TestCapabilityManifestValidateScopesCapabilitiesByAgent(t *testing.T) {
	manifest := CapabilityManifest{Capabilities: []Capability{{Name: "tool:file_write", Agent: "coder"}}}
	available := CapabilityManifest{Capabilities: []Capability{{Name: "tool:file_write", Agent: "chronos-code"}}}
	err := manifest.Validate(available)
	if err == nil || !strings.Contains(err.Error(), `for agent "coder"`) {
		t.Fatalf("Validate() error = %v, want agent-scoped missing capability", err)
	}
}

func TestCapabilityManifestUnmarshalRejectsEmptyName(t *testing.T) {
	var manifest CapabilityManifest
	err := yaml.Unmarshal([]byte("capabilities:\n  - name: '   '\n"), &manifest)
	if err == nil || !strings.Contains(err.Error(), "capabilities[0].name") {
		t.Fatalf("unmarshal error = %v, want empty capability name error", err)
	}
}

func TestCapabilityManifestUnmarshalNormalizesNames(t *testing.T) {
	var manifest CapabilityManifest
	err := yaml.Unmarshal([]byte("capabilities:\n  - name: ' tool:file_read '\n    agent: ' coder '\n"), &manifest)
	if err != nil {
		t.Fatalf("unmarshal error = %v", err)
	}
	got := manifest.Capabilities[0]
	if got.Name != "tool:file_read" || got.Agent != "coder" {
		t.Fatalf("capability = %+v, want normalized name and agent", got)
	}
}

func TestConfigUnmarshalsRuntimeCapabilities(t *testing.T) {
	var cfg Config
	err := yaml.Unmarshal([]byte("runtime_capabilities:\n  capabilities:\n    - name: graph:code\n    - name: tool:file_write\n      agent: coder\n"), &cfg)
	if err != nil {
		t.Fatalf("unmarshal error = %v", err)
	}
	if got := cfg.RuntimeCaps.Capabilities; len(got) != 2 || got[1].Agent != "coder" {
		t.Fatalf("runtime capabilities = %+v, want two parsed requirements", got)
	}
}
