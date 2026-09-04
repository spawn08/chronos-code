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

func TestCapabilityManifestUnmarshalRejectsEmptyName(t *testing.T) {
	var manifest CapabilityManifest
	err := yaml.Unmarshal([]byte("capabilities:\n  - name: '   '\n"), &manifest)
	if err == nil || !strings.Contains(err.Error(), "capabilities[0].name") {
		t.Fatalf("unmarshal error = %v, want empty capability name error", err)
	}
}
