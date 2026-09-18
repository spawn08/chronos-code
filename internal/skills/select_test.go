package skills

import (
	"strings"
	"testing"
)

func testCatalog() []*Skill {
	return []*Skill{
		{Name: "code-review", Description: "Review code for correctness bugs and security issues", Triggers: []string{"review", "pr", "quality"}, Body: "review body"},
		{Name: "test-writer", Description: "Generate tests for existing code", Triggers: []string{"test", "pytest", "coverage"}, Body: "test-writer body"},
		{Name: "git-workflow", Description: "Git operations: commits, branches, PRs", Triggers: []string{"git", "commit", "branch", "pr"}, Body: "git body"},
		{Name: "document", Description: "Generate or improve code documentation", Triggers: []string{"docs", "documentation", "readme"}, Body: "document body"},
	}
}

func TestSelectRanksMostRelevantSkillFirst(t *testing.T) {
	selected := Select("please review this pull request for bugs", testCatalog(), 3, "gpt-4o")
	if len(selected) == 0 {
		t.Fatal("Select returned nothing, want at least code-review")
	}
	if selected[0].Name != "code-review" {
		t.Errorf("selected[0].Name = %q, want code-review (best trigger/description match)", selected[0].Name)
	}
}

func TestSelectRespectsTopK(t *testing.T) {
	selected := Select("review the git commit and write tests, update docs too", testCatalog(), 2, "gpt-4o")
	if len(selected) > 2 {
		t.Fatalf("len(selected) = %d, want <= 2 (topK)", len(selected))
	}
}

func TestSelectNoMatchReturnsNil(t *testing.T) {
	selected := Select("xyzxyz nonsense query zzqx", testCatalog(), 3, "gpt-4o")
	if len(selected) != 0 {
		t.Fatalf("selected = %+v, want none (no term overlap)", selected)
	}
}

func TestSelectEmptyMessageReturnsNil(t *testing.T) {
	selected := Select("", testCatalog(), 3, "gpt-4o")
	if len(selected) != 0 {
		t.Fatalf("selected = %+v, want none for empty message", selected)
	}
}

func TestSelectTrimsToFitTokenBudget(t *testing.T) {
	huge := strings.Repeat("word ", 20000)
	catalog := []*Skill{
		{Name: "a", Description: "review", Triggers: []string{"review"}, Body: huge},
		{Name: "b", Description: "review", Triggers: []string{"review"}, Body: huge},
		{Name: "c", Description: "review", Triggers: []string{"review"}, Body: huge},
	}
	selected := Select("review this", catalog, 3, "gpt-4o")
	rendered := Render(selected)
	if len(selected) >= 3 {
		t.Fatalf("len(selected) = %d, want fewer than topK once budget-trimmed", len(selected))
	}
	if rendered == "" && len(selected) > 0 {
		t.Fatal("Render returned empty for a non-empty selection")
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

func TestSelectWithCapabilitiesSeparatesRationaleFromContext(t *testing.T) {
	catalog := []*Skill{
		{Name: "accepted", Description: "review correctness", ToolsRequired: []string{"file_read"}, ModelHint: "sonnet", Body: "accepted body"},
		{Name: "missing-tool", Description: "review correctness", ToolsRequired: []string{"shell"}, Body: "must not leak"},
		{Name: "wrong-model", Description: "review correctness", ModelHint: "opus", Body: "must not leak either"},
	}
	result := SelectWithCapabilities("review correctness", catalog, 3, CapabilityManifest{
		AgentID: "reviewer", ModelID: "claude-sonnet-4", Tools: map[string]struct{}{"file_read": {}},
	})
	if len(result.Selected) != 1 || result.Selected[0].Name != "accepted" {
		t.Fatalf("selected = %+v", result.Selected)
	}
	if strings.Contains(result.Context, "missing required tools") || strings.Contains(result.Context, "does not satisfy") || strings.Contains(result.Context, "must not leak") {
		t.Fatalf("selection rationale leaked into model context: %q", result.Context)
	}
	if len(result.Decisions) != 3 || result.Decisions[0].Score <= 0 {
		t.Fatalf("decisions = %+v", result.Decisions)
	}
	if result.Decisions[1].Rejection == "" || result.Decisions[2].Rejection == "" {
		t.Fatalf("rejection rationale missing: %+v", result.Decisions)
	}
}
