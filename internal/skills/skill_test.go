package skills

import (
	"os"
	"path/filepath"
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
