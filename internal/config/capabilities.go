package config

import (
	"fmt"
	"strings"
)

// Capability describes one capability required by a manifest. Optional
// capabilities do not prevent a manifest from loading when unavailable.
type Capability struct {
	Name     string `yaml:"name"`
	Optional bool   `yaml:"optional,omitempty"`
}

// CapabilityManifest declares the capabilities needed by a configuration.
type CapabilityManifest struct {
	Capabilities []Capability `yaml:"capabilities"`
}

// Validate checks that every required capability is present in defaults.
func (m CapabilityManifest) Validate(defaults CapabilityManifest) error {
	available := make(map[string]struct{}, len(defaults.Capabilities))
	for _, capability := range defaults.Capabilities {
		available[capability.Name] = struct{}{}
	}

	for _, capability := range m.Capabilities {
		if _, ok := available[capability.Name]; ok || capability.Optional {
			continue
		}
		return fmt.Errorf("required capability %q is not available in defaults", capability.Name)
	}
	return nil
}

// UnmarshalYAML validates each capability name while preserving declaration
// order for deterministic capability selection.
func (m *CapabilityManifest) UnmarshalYAML(unmarshal func(any) error) error {
	type manifest CapabilityManifest
	var decoded manifest
	if err := unmarshal(&decoded); err != nil {
		return err
	}
	for i, capability := range decoded.Capabilities {
		if strings.TrimSpace(capability.Name) == "" {
			return fmt.Errorf("capabilities[%d].name: must not be empty", i)
		}
	}
	*m = CapabilityManifest(decoded)
	return nil
}
