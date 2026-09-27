package skills

import (
	"fmt"
	"sort"
	"strings"
)

// Skills use progressive disclosure: every turn lists each available
// skill's name and description (RenderCatalog), and the model loads a
// skill's body on demand by name (Find), so it, not a keyword ranker,
// judges relevance, and bodies cost context only when used.

// Catalog bounds: a skill's description is its whole selection signal, so
// it is kept, but capped; very large catalogs list the first skills by name.
const (
	MaxCatalogDescription = 300
	MaxCatalogBytes       = 12000
)

type CapabilityManifest struct {
	AgentID string
	ModelID string
	Tools   map[string]struct{}
}

func capabilityRejection(skill *Skill, manifest CapabilityManifest) string {
	if skill == nil {
		return "invalid skill"
	}
	if hint := strings.TrimSpace(strings.ToLower(skill.ModelHint)); hint != "" &&
		!strings.Contains(strings.ToLower(manifest.ModelID), hint) {
		return fmt.Sprintf("model %q does not satisfy hint %q", manifest.ModelID, skill.ModelHint)
	}
	var missing []string
	for _, required := range skill.ToolsRequired {
		if _, ok := manifest.Tools[required]; !ok {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return "missing required tools: " + strings.Join(missing, ", ")
	}
	return ""
}

// CapabilityRejection returns an empty string when skill can run in manifest.
func CapabilityRejection(skill *Skill, manifest CapabilityManifest) string {
	return capabilityRejection(skill, manifest)
}

// Available returns the skills of all that can run in manifest, sorted by
// name so the rendered catalog is stable across turns.
func Available(all []*Skill, manifest CapabilityManifest) []*Skill {
	out := make([]*Skill, 0, len(all))
	for _, skill := range all {
		if capabilityRejection(skill, manifest) == "" {
			out = append(out, skill)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// Find returns the skill named name (case-insensitive), or nil.
func Find(all []*Skill, name string) *Skill {
	name = strings.TrimSpace(name)
	for _, skill := range all {
		if skill != nil && strings.EqualFold(skill.Name, name) {
			return skill
		}
	}
	return nil
}

// RenderCatalog lists available skills for the model, with how to load
// one through toolName. It returns the rendered text and the number of
// skills listed; "" when there is nothing to list.
func RenderCatalog(available []*Skill, toolName string) (string, int) {
	if len(available) == 0 {
		return "", 0
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<skill_catalog>\nSkills are instruction bundles for specific kinds of tasks. "+
		"When a task clearly matches a skill's description, call %s with that skill's name and follow the returned instructions before doing the work. "+
		"Do not load skills that do not clearly apply.\n", toolName)
	listed := 0
	for _, skill := range available {
		line := "- " + skill.Name
		if description := catalogDescription(skill); description != "" {
			line += ": " + description
		}
		if b.Len()+len(line)+len("\n</skill_catalog>") > MaxCatalogBytes {
			break
		}
		b.WriteString(line + "\n")
		listed++
	}
	b.WriteString("</skill_catalog>")
	return b.String(), listed
}

// catalogDescription is a skill's description on one line, falling back to
// its triggers, capped at MaxCatalogDescription bytes on a rune boundary.
func catalogDescription(skill *Skill) string {
	description := strings.Join(strings.Fields(skill.Description), " ")
	if description == "" && len(skill.Triggers) > 0 {
		description = "use for " + strings.Join(skill.Triggers, ", ")
	}
	if len(description) <= MaxCatalogDescription {
		return description
	}
	cut := MaxCatalogDescription
	for cut > 0 && !isRuneStart(description[cut]) {
		cut--
	}
	return strings.TrimSpace(description[:cut]) + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Render formats selected skills as ROADMAP.md §5.1's
// `<skill name="…">…</skill>` blocks, in the order given.
func Render(selected []*Skill) string {
	if len(selected) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range selected {
		fmt.Fprintf(&b, "<skill name=%q>\n%s\n</skill>\n", s.Name, s.Body)
	}
	return strings.TrimSpace(b.String())
}
