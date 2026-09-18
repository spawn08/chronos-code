package defaults

import (
	"io/fs"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCatalogCoversEveryEmbeddedArtifact(t *testing.T) {
	artifacts, err := Catalog()
	if err != nil {
		t.Fatalf("Catalog() error = %v", err)
	}

	byPath := make(map[string]Artifact, len(artifacts))
	for _, artifact := range artifacts {
		if _, exists := byPath[artifact.Path]; exists {
			t.Errorf("duplicate catalog entry %q", artifact.Path)
		}
		if artifact.Activation != RuntimeActive && artifact.Activation != ExportOnly {
			t.Errorf("artifact %q activation = %q", artifact.Path, artifact.Activation)
		}
		if artifact.Rationale == "" {
			t.Errorf("artifact %q has no rationale", artifact.Path)
		}
		byPath[artifact.Path] = artifact
	}

	if err := fs.WalkDir(FS, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if _, ok := byPath[path]; !ok {
			t.Errorf("embedded artifact %q is not cataloged", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk embedded defaults: %v", err)
	}
}

func TestCatalogMarksUnsupportedArtifactsExportOnly(t *testing.T) {
	artifacts, err := Catalog()
	if err != nil {
		t.Fatalf("Catalog() error = %v", err)
	}
	byPath := make(map[string]Artifact, len(artifacts))
	for _, artifact := range artifacts {
		byPath[artifact.Path] = artifact
	}

	for _, path := range []string{"mcp-servers.yaml", "tools.yaml", "teams/code-review.yaml", "teams/debug.yaml"} {
		if got := byPath[path].Activation; got != ExportOnly {
			t.Errorf("%s activation = %q, want %q", path, got, ExportOnly)
		}
	}
	if got := byPath["agents/ppd-planner.yaml"].Activation; got != RuntimeActive {
		t.Errorf("agents/ppd-planner.yaml activation = %q, want %q", got, RuntimeActive)
	}
}

func TestValidateCatalog(t *testing.T) {
	if err := ValidateCatalog(); err != nil {
		t.Fatalf("ValidateCatalog() error = %v", err)
	}
}

func TestRuntimeAgentPromptsDoNotRequireUnavailablePlanningTools(t *testing.T) {
	for _, path := range []string{"agents/chronos-code.yaml", "agents/coder.yaml"} {
		data, err := ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		var document struct {
			SystemPrompt string `yaml:"system_prompt"`
		}
		if err := yaml.Unmarshal(data, &document); err != nil {
			t.Fatalf("parse %q: %v", path, err)
		}
		for _, unavailable := range []string{"update_plan", "fs_write", "fs_read"} {
			if strings.Contains(document.SystemPrompt, unavailable) {
				t.Errorf("runtime prompt %q requires unavailable tool %q", path, unavailable)
			}
		}
	}
}
