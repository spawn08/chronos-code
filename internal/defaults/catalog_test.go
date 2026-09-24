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
	if got := byPath["agents/delivery-strategist.yaml"].Activation; got != RuntimeActive {
		t.Errorf("agents/delivery-strategist.yaml activation = %q, want %q", got, RuntimeActive)
	}
	if _, exists := byPath["agents/ppd-planner.yaml"]; exists {
		t.Error("deprecated ppd-planner remains bundled without a runtime compatibility requirement")
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

type bundledAgentDefinition struct {
	ID           string `yaml:"id"`
	SystemPrompt string `yaml:"system_prompt"`
	Tools        []struct {
		Name       string `yaml:"name"`
		Permission string `yaml:"permission"`
	} `yaml:"tools"`
	SubAgents []string `yaml:"sub_agents"`
}

func TestBundledRuntimeAgentInventoryAndTesterWiring(t *testing.T) {
	data, err := ReadFile("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var configDocument struct {
		Agents []bundledAgentDefinition `yaml:"agents"`
	}
	if err := yaml.Unmarshal(data, &configDocument); err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]bundledAgentDefinition, len(configDocument.Agents)+1)
	for _, definition := range configDocument.Agents {
		byID[definition.ID] = definition
	}
	strategistData, err := ReadFile("agents/delivery-strategist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var strategist bundledAgentDefinition
	if err := yaml.Unmarshal(strategistData, &strategist); err != nil {
		t.Fatal(err)
	}
	byID[strategist.ID] = strategist

	expected := []string{"chronos-code", "coder", "planner", "delivery-strategist", "reviewer", "debugger", "researcher", "architect", "explainer", "tester"}
	if len(byID) != len(expected) {
		t.Fatalf("bundled agent count = %d, want %d: %#v", len(byID), len(expected), byID)
	}
	for _, id := range expected {
		if _, ok := byID[id]; !ok {
			t.Errorf("bundled agent %q is not loaded", id)
		}
	}
	for _, parentID := range []string{"chronos-code", "coder"} {
		if !containsString(byID[parentID].SubAgents, "tester") {
			t.Errorf("%s subagents = %v, want tester", parentID, byID[parentID].SubAgents)
		}
	}

	tester := byID["tester"]
	testerData, err := ReadFile("agents/tester.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var testerPrompt bundledAgentDefinition
	if err := yaml.Unmarshal(testerData, &testerPrompt); err != nil {
		t.Fatal(err)
	}
	prompt := strings.ToLower(testerPrompt.SystemPrompt)
	if testerPrompt.SystemPrompt == "" || !strings.Contains(prompt, "must not intentionally edit source") || !strings.Contains(prompt, "may create artifacts") || !strings.Contains(prompt, "not a hard read-only sandbox") || !strings.Contains(testerPrompt.SystemPrompt, "EVIDENCE:") {
		t.Errorf("tester prompt does not state the verification and shell contract")
	}
	toolNames := make([]string, 0, len(tester.Tools))
	for _, configuredTool := range tester.Tools {
		toolNames = append(toolNames, configuredTool.Name)
		if configuredTool.Name == "shell" && configuredTool.Permission != "require_approval" {
			t.Errorf("bundled tester shell permission = %q, want require_approval", configuredTool.Permission)
		}
	}
	if !containsString(toolNames, "shell") || containsString(toolNames, "file_write") {
		t.Errorf("tester tools = %v, want shell and no file_write", toolNames)
	}

	testerYAMLTools := make([]string, 0, len(testerPrompt.Tools))
	for _, configuredTool := range testerPrompt.Tools {
		testerYAMLTools = append(testerYAMLTools, configuredTool.Name)
		if configuredTool.Name == "shell" && configuredTool.Permission != "require_approval" {
			t.Errorf("tester YAML shell permission = %q, want require_approval", configuredTool.Permission)
		}
	}
	if !containsString(testerYAMLTools, "shell") || containsString(testerYAMLTools, "file_write") {
		t.Errorf("tester YAML tools = %v, want shell and no file_write", testerYAMLTools)
	}
}

func TestSpecialistPromptsUseStructuredSpawnedHandoffs(t *testing.T) {
	// delivery-strategist is intentionally excluded: its output is parsed as an exact,
	// runtime-owned decomposition JSON schema rather than a conversational handoff.
	for _, id := range []string{"coder", "planner", "reviewer", "debugger", "researcher", "architect", "explainer", "tester"} {
		data, err := ReadFile("agents/" + id + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		var definition bundledAgentDefinition
		if err := yaml.Unmarshal(data, &definition); err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"complete|partial|blocked|failed", "EVIDENCE", "UNRESOLVED WORK", "NEXT ACTION", "direct"} {
			if !strings.Contains(definition.SystemPrompt, required) {
				t.Errorf("%s prompt missing handoff contract text %q", id, required)
			}
		}
	}
}

func TestDeliveryStrategistPromptIsBoundedAndRuntimeHonest(t *testing.T) {
	data, err := ReadFile("agents/delivery-strategist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var definition bundledAgentDefinition
	if err := yaml.Unmarshal(data, &definition); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"next useful work frontier", "1 to 6 nodes", "Kind must be investigate, decide, implement, verify, or integrate", "actual artifact or ordering requirements", "never edit code", "not evidence of execution or completion", "claim that rolling replanning occurs automatically"} {
		if !strings.Contains(definition.SystemPrompt, required) {
			t.Errorf("delivery strategist prompt missing %q", required)
		}
	}
}

func TestSpecialistPromptsAvoidUnsupportedBehaviorClaims(t *testing.T) {
	checks := map[string][]string{
		"debugger":   {"apply minimal fixes", "root cause is at the bottom", "never the whole file", "After fixing"},
		"planner":    {"always leaf-first", "Always start with multi_resolution_view", "impact_analysis on every file"},
		"reviewer":   {"stop reviewing and report it immediately"},
		"researcher": {"Always try these first", "Full file reads never"},
		"architect":  {"Always start with multi_resolution_view"},
		"explainer":  {"graph_query first"},
	}
	for id, forbidden := range checks {
		data, err := ReadFile("agents/" + id + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range forbidden {
			if strings.Contains(string(data), text) {
				t.Errorf("%s prompt retains forbidden instruction %q", id, text)
			}
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
