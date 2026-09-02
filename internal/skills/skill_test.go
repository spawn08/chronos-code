package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadBundledYAML(t *testing.T) {
	data := []byte(`
skills:
  - name: code-review
    version: "1.0.0"
    description: "Review code for bugs"
    author: chronos-code
    tags: [review, quality]
    tools: [file_read, shell]
    manifest: |
      ## Code Review
      Check for bugs.
`)
	got, err := LoadBundledYAML(data)
	if err != nil {
		t.Fatalf("LoadBundledYAML: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	s := got[0]
	if s.Name != "code-review" || s.Description != "Review code for bugs" {
		t.Errorf("skill = %+v", s)
	}
	if len(s.Triggers) != 2 || s.Triggers[0] != "review" {
		t.Errorf("Triggers = %v, want tags mapped through", s.Triggers)
	}
	if !strings.Contains(s.Body, "Check for bugs") {
		t.Errorf("Body = %q, want manifest content", s.Body)
	}
	if s.Source != "bundled:code-review" {
		t.Errorf("Source = %q, want bundled:code-review", s.Source)
	}
}

func TestLoadDirParsesSkillMDFrontmatter(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "python-testing")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: python-testing
description: When to use pytest, uv, ruff for Python projects.
version: 1.0.0
triggers: [pytest, python test, uv, ruff]
model_hint: sonnet
tools_required: [read, write, bash]
---
# Python Testing

Use pytest with uv for dependency management.
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	s := got[0]
	if s.Name != "python-testing" {
		t.Errorf("Name = %q", s.Name)
	}
	if len(s.Triggers) != 4 || s.Triggers[0] != "pytest" {
		t.Errorf("Triggers = %v", s.Triggers)
	}
	if s.ModelHint != "sonnet" {
		t.Errorf("ModelHint = %q, want sonnet", s.ModelHint)
	}
	if len(s.ToolsRequired) != 3 {
		t.Errorf("ToolsRequired = %v", s.ToolsRequired)
	}
	if !strings.Contains(s.Body, "Use pytest with uv") {
		t.Errorf("Body = %q", s.Body)
	}
	if s.Source != filepath.Join(skillDir, "SKILL.md") {
		t.Errorf("Source = %q", s.Source)
	}
}

func TestLoadDirMissingDirIsNotError(t *testing.T) {
	got, err := LoadDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if got != nil {
		t.Errorf("got = %v, want nil", got)
	}
}

func TestLoadDirRejectsMissingFrontmatterName(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "bad")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\ndescription: no name\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("LoadDir: want error for SKILL.md missing a name")
	}
}

func TestDiscoverRepoTierShadowsBundled(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from any real ~/.chronos-code/skills on the host.
	root := t.TempDir()
	skillDir := filepath.Join(root, ".chronos-code", "skills", "code-review")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: code-review\ndescription: repo override\n---\ncustom body"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	bundled := []*Skill{{Name: "code-review", Description: "bundled version", Body: "bundled body"}}
	merged, err := Discover(root, bundled)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("len(merged) = %d, want 1 (repo tier shadows bundled)", len(merged))
	}
	if merged[0].Description != "repo override" {
		t.Errorf("Description = %q, want repo tier to win", merged[0].Description)
	}
}

func TestDiscoverFallsBackToBundledWhenNoOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from any real ~/.chronos-code/skills on the host.
	root := t.TempDir()
	bundled := []*Skill{{Name: "code-review", Description: "bundled version"}}
	merged, err := Discover(root, bundled)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(merged) != 1 || merged[0].Description != "bundled version" {
		t.Fatalf("merged = %+v, want bundled fallback", merged)
	}
}

func TestDiscoverMergesAllTiersWithPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()

	writeSkill(t, filepath.Join(root, ".chronos-code", "skills"), "shared", "repo")
	writeSkill(t, filepath.Join(root, ".chronos-code", "skills"), "repo-only", "repo")
	writeSkill(t, filepath.Join(home, ".chronos-code", "skills"), "shared", "user")
	writeSkill(t, filepath.Join(home, ".chronos-code", "skills"), "user-wins", "user")
	writeSkill(t, filepath.Join(home, ".chronos-code", "skills"), "user-only", "user")
	pluginSkills := filepath.Join(home, ".chronos-code", "plugins", "plugin-a", "skills")
	writeSkill(t, pluginSkills, "shared", "plugin")
	writeSkill(t, pluginSkills, "user-wins", "plugin")
	writeSkill(t, pluginSkills, "plugin-wins", "plugin")
	writeSkill(t, pluginSkills, "plugin-only", "plugin")

	bundled := []*Skill{
		{Name: "shared", Description: "bundled"},
		{Name: "user-wins", Description: "bundled"},
		{Name: "plugin-wins", Description: "bundled"},
		{Name: "bundled-only", Description: "bundled"},
	}
	merged, err := Discover(root, bundled)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	got := make(map[string]string, len(merged))
	for _, skill := range merged {
		got[skill.Name] = skill.Description
	}
	want := map[string]string{
		"shared":       "repo",
		"repo-only":    "repo",
		"user-wins":    "user",
		"user-only":    "user",
		"plugin-wins":  "plugin",
		"plugin-only":  "plugin",
		"bundled-only": "bundled",
	}
	if len(got) != len(want) {
		t.Fatalf("skills = %v, want %v", got, want)
	}
	for name, description := range want {
		if got[name] != description {
			t.Errorf("skill %q description = %q, want %q", name, got[name], description)
		}
	}
}

func TestDiscoverProviderDirectoriesAndRemovesDuplicates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()

	writeSkill(t, filepath.Join(root, ".chronos-code", "skills"), "shared", "chronos project")
	writeSkill(t, filepath.Join(root, ".claude", "skills"), "Shared", "claude duplicate")
	writeSkill(t, filepath.Join(root, ".codex", "skills"), "codex-only", "codex project")
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "agents-only", "agents project")
	writeSkill(t, filepath.Join(root, ".gemini", "skills"), "gemini-only", "gemini project")
	writeSkill(t, filepath.Join(root, ".opencode", "skills"), "opencode-only", "opencode project")
	writeSkill(t, filepath.Join(root, ".cursor", "skills"), "cursor-only", "cursor project")
	writeSkill(t, filepath.Join(root, ".windsurf", "skills"), "windsurf-only", "windsurf project")
	writeSkill(t, filepath.Join(root, ".github", "skills"), "copilot-only", "github project")
	userDirs := []string{
		filepath.Join(".claude", "skills"), filepath.Join(".codex", "skills"),
		filepath.Join(".agents", "skills"), filepath.Join(".gemini", "skills"),
		filepath.Join(".opencode", "skills"), filepath.Join(".config", "opencode", "skills"),
		filepath.Join(".cursor", "skills"), filepath.Join(".windsurf", "skills"),
		filepath.Join(".github", "skills"),
	}
	for i, dir := range userDirs {
		writeSkill(t, filepath.Join(home, dir), fmt.Sprintf("user-%d", i), "user provider")
	}

	merged, err := Discover(root, []*Skill{{Name: "CODEX-ONLY", Description: "bundled duplicate"}})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	got := make(map[string]string, len(merged))
	for _, skill := range merged {
		key := strings.ToLower(skill.Name)
		if _, exists := got[key]; exists {
			t.Fatalf("duplicate skill %q in merged catalog", key)
		}
		got[key] = skill.Description
	}
	want := map[string]string{
		"shared": "chronos project", "codex-only": "codex project", "agents-only": "agents project",
		"gemini-only": "gemini project", "opencode-only": "opencode project", "cursor-only": "cursor project",
		"windsurf-only": "windsurf project", "copilot-only": "github project",
	}
	for i := range userDirs {
		want[fmt.Sprintf("user-%d", i)] = "user provider"
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("discovered skills = %v, want %v", got, want)
	}
}

func TestDiscoverTraversesPluginsDeterministically(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pluginsDir := filepath.Join(home, ".chronos-code", "plugins")

	writeSkill(t, filepath.Join(pluginsDir, "zeta", "skills"), "collision", "zeta")
	writeSkill(t, filepath.Join(pluginsDir, "zeta", "skills"), "zeta-only", "zeta")
	writeSkill(t, filepath.Join(pluginsDir, "alpha", "skills"), "collision", "alpha")
	writeSkill(t, filepath.Join(pluginsDir, "alpha", "skills"), "alpha-only", "alpha")

	merged, err := Discover(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	wantNames := []string{"alpha-only", "collision", "zeta-only"}
	if len(merged) != len(wantNames) {
		t.Fatalf("len(merged) = %d, want %d", len(merged), len(wantNames))
	}
	for i, name := range wantNames {
		if merged[i].Name != name {
			t.Errorf("merged[%d].Name = %q, want %q", i, merged[i].Name, name)
		}
	}
	if merged[1].Description != "alpha" {
		t.Errorf("collision description = %q, want first lexical plugin alpha", merged[1].Description)
	}
}

func TestDiscoverMissingPluginDirectoryIsNotError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bundled := []*Skill{{Name: "bundled", Description: "fallback"}}

	merged, err := Discover(t.TempDir(), bundled)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(merged) != 1 || merged[0] != bundled[0] {
		t.Fatalf("merged = %+v, want bundled fallback", merged)
	}
}

func writeSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\nbody"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
