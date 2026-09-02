// Package skills implements chronos-code's own skill discovery and
// selection (ROADMAP.md §5.1), independent of chronos SDK's SkillConfig,
// which bakes every configured skill's full manifest into the system
// prompt at build time — exactly the "load everything into context"
// anti-pattern §5.1 explicitly rejects ("Do not load all skills into
// context. That's Claude Code's biggest complaint from power users.").
// This package instead selects a small, relevant, token-budgeted subset
// per turn (see select.go), for dynamic injection via an agent's
// ContextPinsFn.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill is chronos-code's own skill representation: a named capability
// bundle with trigger keywords for selection matching (ROADMAP.md §5.1's
// SKILL.md frontmatter schema) and a markdown body injected verbatim when
// selected.
type Skill struct {
	Name          string
	Version       string
	Description   string
	Author        string
	Triggers      []string
	ModelHint     string
	ToolsRequired []string
	Body          string
	// Source identifies where this Skill was loaded from (an absolute
	// SKILL.md path, or "bundled:<name>" for the embedded catalog), for
	// diagnostics only.
	Source string
}

// text returns the combined triggers+description+name text Select scores
// against a user message — the PRD's "BM25 over triggers + description."
func (s *Skill) text() string {
	return s.Name + " " + s.Description + " " + strings.Join(s.Triggers, " ")
}

// bundledYAML mirrors internal/defaults/skills/default-skills.yaml's shape:
// a single file declaring multiple skills, with `tags` doubling as trigger
// keywords and `manifest` as the body.
type bundledYAML struct {
	Skills []struct {
		Name        string   `yaml:"name"`
		Version     string   `yaml:"version"`
		Description string   `yaml:"description"`
		Author      string   `yaml:"author"`
		Tags        []string `yaml:"tags"`
		Tools       []string `yaml:"tools"`
		Manifest    string   `yaml:"manifest"`
	} `yaml:"skills"`
}

// LoadBundledYAML parses data in default-skills.yaml's format into Skills,
// tagged with source "bundled:<name>".
func LoadBundledYAML(data []byte) ([]*Skill, error) {
	var doc bundledYAML
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("skills: parse bundled catalog: %w", err)
	}
	out := make([]*Skill, 0, len(doc.Skills))
	for _, s := range doc.Skills {
		out = append(out, &Skill{
			Name:          s.Name,
			Version:       s.Version,
			Description:   s.Description,
			Author:        s.Author,
			Triggers:      s.Tags,
			ToolsRequired: s.Tools,
			Body:          strings.TrimSpace(s.Manifest),
			Source:        "bundled:" + s.Name,
		})
	}
	return out, nil
}

// skillMDFrontmatter mirrors the SKILL.md frontmatter schema from
// ROADMAP.md §5.1.
type skillMDFrontmatter struct {
	Name          string   `yaml:"name"`
	Description   string   `yaml:"description"`
	Version       string   `yaml:"version"`
	Triggers      []string `yaml:"triggers"`
	ModelHint     string   `yaml:"model_hint"`
	ToolsRequired []string `yaml:"tools_required"`
}

// parseSkillMD splits a SKILL.md file's YAML frontmatter (delimited by a
// "---" line immediately at the start of the file and a closing "---"
// line) from its markdown body. A file with no frontmatter delimiters is
// treated as a body-only skill with no name, which ParseSkillMD's caller
// rejects.
func parseSkillMD(data []byte) (*Skill, error) {
	text := string(data)
	if !strings.HasPrefix(text, "---") {
		return nil, fmt.Errorf("skills: SKILL.md must start with a \"---\" frontmatter delimiter")
	}
	rest := text[3:]
	end := strings.Index(rest, "\n---")
	if end == -1 {
		return nil, fmt.Errorf("skills: SKILL.md frontmatter has no closing \"---\" delimiter")
	}
	fm := rest[:end]
	body := rest[end+len("\n---"):]
	body = strings.TrimLeft(body, "\r\n")

	var meta skillMDFrontmatter
	if err := yaml.Unmarshal([]byte(fm), &meta); err != nil {
		return nil, fmt.Errorf("skills: parse SKILL.md frontmatter: %w", err)
	}
	if strings.TrimSpace(meta.Name) == "" {
		return nil, fmt.Errorf("skills: SKILL.md frontmatter missing required \"name\" field")
	}
	return &Skill{
		Name:          meta.Name,
		Version:       meta.Version,
		Description:   meta.Description,
		Triggers:      meta.Triggers,
		ModelHint:     meta.ModelHint,
		ToolsRequired: meta.ToolsRequired,
		Body:          strings.TrimSpace(body),
	}, nil
}

// LoadDir loads every "<dir>/*/SKILL.md" file into a Skill, per ROADMAP.md
// §5.1's on-disk layout (one directory per skill, e.g.
// "<dir>/python-testing/SKILL.md"). A missing dir is not an error — it
// yields an empty slice, since "no skills directory yet" is the common
// case for both the repo-local and user-global tiers.
func LoadDir(dir string) ([]*Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("skills: read dir %q: %w", dir, err)
	}
	var out []*Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			continue // no SKILL.md in this subdirectory; not an error.
		}
		s, err := parseSkillMD(data)
		if err != nil {
			return nil, fmt.Errorf("skills: %s: %w", path, err)
		}
		s.Source = path
		out = append(out, s)
	}
	return out, nil
}

// Discover merges every tier of ROADMAP.md §5.1's resolution order,
// highest-priority first: repo-local ("<root>/.chronos-code/skills"), user
// global ("~/.chronos-code/skills"), plugin-installed
// ("~/.chronos-code/plugins/*/skills"), then bundled (embedded
// default-skills.yaml, passed in as bundled since internal/defaults isn't
// importable from here without an import cycle risk — callers already have
// defaults.ReadFile). A name present in a higher tier shadows the same name
// in a lower one.
func Discover(root string, bundled []*Skill) ([]*Skill, error) {
	var tiers [][]*Skill

	if root != "" {
		repoSkills, err := LoadDir(filepath.Join(root, ".chronos-code", "skills"))
		if err != nil {
			return nil, err
		}
		tiers = append(tiers, repoSkills)
	}

	if home, err := os.UserHomeDir(); err == nil {
		userSkills, err := LoadDir(filepath.Join(home, ".chronos-code", "skills"))
		if err != nil {
			return nil, err
		}
		tiers = append(tiers, userSkills)

		pluginsDir := filepath.Join(home, ".chronos-code", "plugins")
		entries, err := os.ReadDir(pluginsDir)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("skills: read plugins dir %q: %w", pluginsDir, err)
		}
		var pluginNames []string
		for _, entry := range entries {
			if entry.IsDir() {
				pluginNames = append(pluginNames, entry.Name())
			}
		}
		sort.Strings(pluginNames)
		for _, name := range pluginNames {
			pluginSkills, err := LoadDir(filepath.Join(pluginsDir, name, "skills"))
			if err != nil {
				return nil, err
			}
			tiers = append(tiers, pluginSkills)
		}
	}

	tiers = append(tiers, bundled)

	seen := make(map[string]bool)
	var merged []*Skill
	for _, tier := range tiers {
		for _, s := range tier {
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			merged = append(merged, s)
		}
	}
	return merged, nil
}
