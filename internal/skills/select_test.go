package skills

import (
	"strings"
	"testing"
)

func TestAvailableFiltersByCapabilityAndSortsByName(t *testing.T) {
	catalog := []*Skill{
		{Name: "zeta", Description: "last"},
		{Name: "missing-tool", Description: "x", ToolsRequired: []string{"shell"}},
		{Name: "wrong-model", Description: "x", ModelHint: "opus"},
		{Name: "Alpha", Description: "first", ToolsRequired: []string{"file_read"}, ModelHint: "sonnet"},
	}
	got := Available(catalog, CapabilityManifest{ModelID: "claude-sonnet-4", Tools: map[string]struct{}{"file_read": {}}})
	if len(got) != 2 || got[0].Name != "Alpha" || got[1].Name != "zeta" {
		t.Fatalf("Available = %+v, want [Alpha zeta]", got)
	}
}

func TestFindIsCaseInsensitive(t *testing.T) {
	catalog := []*Skill{{Name: "code-review"}}
	if Find(catalog, " Code-Review ") != catalog[0] {
		t.Fatal("Find did not match case-insensitively")
	}
	if Find(catalog, "missing") != nil {
		t.Fatal("Find matched an unknown name")
	}
}

func TestRenderCatalogListsNamesAndDescriptionsNotBodies(t *testing.T) {
	out, listed := RenderCatalog([]*Skill{
		{Name: "code-review", Description: "Review code\n  for bugs", Body: "BODY MUST NOT APPEAR"},
		{Name: "git-workflow", Triggers: []string{"git", "commit"}},
	}, "skill")
	if listed != 2 {
		t.Fatalf("listed = %d, want 2", listed)
	}
	for _, want := range []string{"<skill_catalog>", "call skill with that skill's name", "- code-review: Review code for bugs", "- git-workflow: use for git, commit", "</skill_catalog>"} {
		if !strings.Contains(out, want) {
			t.Errorf("catalog missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "BODY MUST NOT APPEAR") {
		t.Fatalf("catalog leaked a skill body:\n%s", out)
	}
	if empty, n := RenderCatalog(nil, "skill"); empty != "" || n != 0 {
		t.Fatalf("empty catalog = (%q, %d)", empty, n)
	}
}

func TestRenderCatalogBoundsDescriptionsAndSize(t *testing.T) {
	long := strings.Repeat("é", MaxCatalogDescription)
	out, _ := RenderCatalog([]*Skill{{Name: "long", Description: long}}, "skill")
	line := out[strings.Index(out, "- long: "):]
	line = line[:strings.IndexByte(line, '\n')]
	if len(line) > len("- long: ")+MaxCatalogDescription+len("…") || !strings.HasSuffix(line, "…") {
		t.Fatalf("description not capped on a rune boundary: %q", line)
	}

	var many []*Skill
	for i := 0; i < 200; i++ {
		many = append(many, &Skill{Name: "skill-" + strings.Repeat("x", 10) + string(rune('a'+i%26)), Description: strings.Repeat("d", 200)})
	}
	out, listed := RenderCatalog(many, "skill")
	if len(out) > MaxCatalogBytes || listed == 0 || listed == len(many) || !strings.HasSuffix(out, "</skill_catalog>") {
		t.Fatalf("catalog bytes=%d listed=%d of %d", len(out), listed, len(many))
	}
}

func TestRenderFormatsSkillTags(t *testing.T) {
	out := Render([]*Skill{{Name: "code-review", Body: "do the review"}})
	if !strings.Contains(out, `<skill name="code-review">`) {
		t.Errorf("Render output missing opening tag: %q", out)
	}
	if !strings.Contains(out, "</skill>") {
		t.Errorf("Render output missing closing tag: %q", out)
	}
	if !strings.Contains(out, "do the review") {
		t.Errorf("Render output missing body: %q", out)
	}
}

func TestRenderEmptySelectionReturnsEmptyString(t *testing.T) {
	if out := Render(nil); out != "" {
		t.Errorf("Render(nil) = %q, want empty string", out)
	}
}
