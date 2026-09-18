package config

import (
	"fmt"
	"strings"
)

// Capability describes one capability required by a manifest. Optional
// capabilities do not prevent a manifest from loading when unavailable.
type Capability struct {
	Name     string `yaml:"name"`
	Agent    string `yaml:"agent,omitempty"`
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
		available[capabilityKey(capability)] = struct{}{}
	}

	for _, capability := range m.Capabilities {
		if _, ok := available[capabilityKey(capability)]; ok || capability.Optional {
			continue
		}
		if capability.Agent != "" {
			return fmt.Errorf("required capability %q is not available for agent %q", capability.Name, capability.Agent)
		}
		return fmt.Errorf("required capability %q is not available", capability.Name)
	}
	return nil
}

func capabilityKey(capability Capability) string {
	return capability.Agent + "\x00" + capability.Name
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
		decoded.Capabilities[i].Name = strings.TrimSpace(capability.Name)
		decoded.Capabilities[i].Agent = strings.TrimSpace(capability.Agent)
		if decoded.Capabilities[i].Name == "" {
			return fmt.Errorf("capabilities[%d].name: must not be empty", i)
		}
	}
	*m = CapabilityManifest(decoded)
	return nil
}
