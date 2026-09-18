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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	MaxSkillFileBytes = 256 << 10
	MaxSkillsPerDir   = 256
	MaxPluginDirs     = 128
	MaxTriggers       = 64
	MaxToolsRequired  = 64
)

type Diagnostic struct {
	Source  string
	Message string
}

type Discovery struct {
	Skills      []*Skill
	Diagnostics []Diagnostic
}

var (
	projectSkillDirs = []string{
		filepath.Join(".claude", "skills"),
		filepath.Join(".codex", "skills"),
		filepath.Join(".agents", "skills"),
		filepath.Join(".gemini", "skills"),
		filepath.Join(".opencode", "skills"),
		filepath.Join(".opencode", "skill"),
		filepath.Join(".cursor", "skills"),
		filepath.Join(".windsurf", "skills"),
		filepath.Join(".github", "skills"),
	}
	userSkillDirs = []string{
		filepath.Join(".claude", "skills"),
		filepath.Join(".codex", "skills"),
		filepath.Join(".agents", "skills"),
		filepath.Join(".gemini", "skills"),
		filepath.Join(".opencode", "skills"),
		filepath.Join(".opencode", "skill"),
		filepath.Join(".config", "opencode", "skills"),
		filepath.Join(".config", "opencode", "skill"),
		filepath.Join(".cursor", "skills"),
		filepath.Join(".windsurf", "skills"),
		filepath.Join(".github", "skills"),
	}
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
type bundledSkill struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description"`
	Author      string   `yaml:"author"`
	Tags        []string `yaml:"tags"`
	Tools       []string `yaml:"tools"`
	Manifest    string   `yaml:"manifest"`
}

// LoadBundledYAML parses data in default-skills.yaml's format into Skills,
// tagged with source "bundled:<name>".
func LoadBundledYAML(data []byte) ([]*Skill, error) {
	result, err := LoadBundledYAMLReport(data)
	return result.Skills, err
}

func LoadBundledYAMLReport(data []byte) (Discovery, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return Discovery{}, fmt.Errorf("skills: parse bundled catalog: %w", err)
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return Discovery{}, fmt.Errorf("skills: bundled catalog must be a mapping")
	}
	var entries *yaml.Node
	root := document.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "skills" {
			entries = root.Content[i+1]
			break
		}
	}
	if entries == nil || entries.Kind != yaml.SequenceNode {
		return Discovery{}, fmt.Errorf("skills: bundled catalog skills must be a sequence")
	}
	result := Discovery{Skills: make([]*Skill, 0, len(entries.Content))}
	for i, entry := range entries.Content {
		if unknown := unknownBundledField(entry); unknown != "" {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Source: fmt.Sprintf("bundled:index-%d", i), Message: fmt.Sprintf("unknown field %q", unknown)})
			continue
		}
		var s bundledSkill
		if err := entry.Decode(&s); err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Source: fmt.Sprintf("bundled:index-%d", i), Message: err.Error()})
			continue
		}
		skill := &Skill{
			Name:          s.Name,
			Version:       s.Version,
			Description:   s.Description,
			Author:        s.Author,
			Triggers:      s.Tags,
			ToolsRequired: s.Tools,
			Body:          strings.TrimSpace(s.Manifest),
			Source:        "bundled:" + s.Name,
		}
		if err := validateSkill(skill); err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Source: skill.Source, Message: err.Error()})
			continue
		}
		result.Skills = append(result.Skills, skill)
	}
	return result, nil
}

func unknownBundledField(entry *yaml.Node) string {
	if entry.Kind != yaml.MappingNode {
		return "<non-mapping entry>"
	}
	allowed := map[string]bool{
		"name": true, "version": true, "description": true, "author": true,
		"tags": true, "tools": true, "manifest": true,
	}
	for i := 0; i+1 < len(entry.Content); i += 2 {
		if !allowed[entry.Content[i].Value] {
			return entry.Content[i].Value
		}
	}
	return ""
}

// skillMDFrontmatter mirrors the SKILL.md frontmatter schema from
// ROADMAP.md §5.1.
type skillMDFrontmatter struct {
	Name          string         `yaml:"name"`
	Description   string         `yaml:"description"`
	Version       string         `yaml:"version"`
	Triggers      []string       `yaml:"triggers"`
	ModelHint     string         `yaml:"model_hint"`
	ToolsRequired []string       `yaml:"tools_required"`
	License       string         `yaml:"license"`
	Compatibility any            `yaml:"compatibility"`
	Metadata      map[string]any `yaml:"metadata"`
}

// parseSkillMD splits a SKILL.md file's YAML frontmatter (delimited by a
// "---" line immediately at the start of the file and a closing "---"
// line) from its markdown body. A file with no frontmatter delimiters is
// treated as a body-only skill with no name, which ParseSkillMD's caller
// rejects.
func parseSkillMD(data []byte) (*Skill, error) {
	if len(data) > MaxSkillFileBytes {
		return nil, fmt.Errorf("skills: SKILL.md exceeds %d bytes", MaxSkillFileBytes)
	}
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
	decoder := yaml.NewDecoder(strings.NewReader(fm))
	decoder.KnownFields(true)
	if err := decoder.Decode(&meta); err != nil {
		return nil, fmt.Errorf("skills: parse SKILL.md frontmatter: %w", err)
	}
	if strings.TrimSpace(meta.Name) == "" {
		return nil, fmt.Errorf("skills: SKILL.md frontmatter missing required \"name\" field")
	}
	if len(meta.Triggers) > MaxTriggers {
		return nil, fmt.Errorf("skills: SKILL.md has %d triggers; maximum is %d", len(meta.Triggers), MaxTriggers)
	}
	if len(meta.ToolsRequired) > MaxToolsRequired {
		return nil, fmt.Errorf("skills: SKILL.md has %d required tools; maximum is %d", len(meta.ToolsRequired), MaxToolsRequired)
	}
	skill := &Skill{
		Name:          meta.Name,
		Version:       meta.Version,
		Description:   meta.Description,
		Triggers:      meta.Triggers,
		ModelHint:     meta.ModelHint,
		ToolsRequired: meta.ToolsRequired,
		Body:          strings.TrimSpace(body),
	}
	if err := validateSkill(skill); err != nil {
		return nil, err
	}
	return skill, nil
}

func validateSkill(skill *Skill) error {
	if skill == nil || strings.TrimSpace(skill.Name) == "" {
		return fmt.Errorf("skill name is required")
	}
	if strings.TrimSpace(skill.Name) != skill.Name || strings.ContainsAny(skill.Name, "\r\n\t<>") {
		return fmt.Errorf("skill name %q contains unsafe characters", skill.Name)
	}
	if len(skill.Triggers) > MaxTriggers {
		return fmt.Errorf("skill %q has %d triggers; maximum is %d", skill.Name, len(skill.Triggers), MaxTriggers)
	}
	if len(skill.ToolsRequired) > MaxToolsRequired {
		return fmt.Errorf("skill %q has %d required tools; maximum is %d", skill.Name, len(skill.ToolsRequired), MaxToolsRequired)
	}
	if len(skill.Body) > MaxSkillFileBytes {
		return fmt.Errorf("skill %q body exceeds %d bytes", skill.Name, MaxSkillFileBytes)
	}
	return nil
}

// LoadDir loads every "<dir>/*/SKILL.md" file into a Skill, per ROADMAP.md
// §5.1's on-disk layout (one directory per skill, e.g.
// "<dir>/python-testing/SKILL.md"). A missing dir is not an error — it
// yields an empty slice, since "no skills directory yet" is the common
// case for both the repo-local and user-global tiers.
func LoadDir(dir string) ([]*Skill, error) {
	result, err := LoadDirReport(dir)
	return result.Skills, err
}

// LoadDirReport isolates invalid files and returns diagnostics without
// suppressing valid sibling skills.
func LoadDirReport(dir string) (Discovery, error) {
	entries, exceeded, err := readDirBounded(dir, MaxSkillsPerDir)
	if err != nil {
		if os.IsNotExist(err) {
			return Discovery{}, nil
		}
		return Discovery{}, fmt.Errorf("skills: read dir %q: %w", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result := Discovery{}
	if exceeded {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{Source: dir, Message: fmt.Sprintf("skill directory exceeds %d entries", MaxSkillsPerDir)})
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		info, err := os.Lstat(path)
		if err != nil {
			continue // no SKILL.md in this subdirectory; not an error.
		}
		if !info.Mode().IsRegular() {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Source: path, Message: "SKILL.md must be a regular file"})
			continue
		}
		if info.Size() > MaxSkillFileBytes {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Source: path, Message: fmt.Sprintf("SKILL.md exceeds %d bytes", MaxSkillFileBytes)})
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Source: path, Message: err.Error()})
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(file, MaxSkillFileBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Source: path, Message: fmt.Errorf("read skill: %w", errors.Join(readErr, closeErr)).Error()})
			continue
		}
		s, err := parseSkillMD(data)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Source: path, Message: err.Error()})
			continue
		}
		s.Source = path
		result.Skills = append(result.Skills, s)
	}
	return result, nil
}

// Discover merges native and provider skill directories in highest-priority
// order: project chronos-code, project provider directories, user
// chronos-code, user provider directories, plugins, then bundled defaults.
// Names are matched case-insensitively so a skill is injected only once even
// when several providers expose the same installation.
func Discover(root string, bundled []*Skill) ([]*Skill, error) {
	result, err := DiscoverReport(root, bundled)
	return result.Skills, err
}

// DiscoverReport merges all tiers while retaining file-level diagnostics.
func DiscoverReport(root string, bundled []*Skill) (Discovery, error) {
	var tiers [][]*Skill
	var diagnostics []Diagnostic

	if root != "" {
		for _, dir := range append([]string{filepath.Join(".chronos-code", "skills")}, projectSkillDirs...) {
			loaded, err := LoadDirReport(filepath.Join(root, dir))
			if err != nil {
				return Discovery{}, err
			}
			tiers = append(tiers, loaded.Skills)
			diagnostics = append(diagnostics, loaded.Diagnostics...)
		}
	}

	if home, err := os.UserHomeDir(); err == nil {
		for _, dir := range append([]string{filepath.Join(".chronos-code", "skills")}, userSkillDirs...) {
			loaded, err := LoadDirReport(filepath.Join(home, dir))
			if err != nil {
				return Discovery{}, err
			}
			tiers = append(tiers, loaded.Skills)
			diagnostics = append(diagnostics, loaded.Diagnostics...)
		}

		pluginsDir := filepath.Join(home, ".chronos-code", "plugins")
		entries, exceeded, err := readDirBounded(pluginsDir, MaxPluginDirs)
		if err != nil && !os.IsNotExist(err) {
			return Discovery{}, fmt.Errorf("skills: read plugins dir %q: %w", pluginsDir, err)
		}
		if exceeded {
			diagnostics = append(diagnostics, Diagnostic{Source: pluginsDir, Message: fmt.Sprintf("plugin directory exceeds %d entries", MaxPluginDirs)})
		}
		var pluginNames []string
		for _, entry := range entries {
			if entry.IsDir() {
				pluginNames = append(pluginNames, entry.Name())
			}
		}
		sort.Strings(pluginNames)
		for _, name := range pluginNames {
			loaded, err := LoadDirReport(filepath.Join(pluginsDir, name, "skills"))
			if err != nil {
				return Discovery{}, err
			}
			tiers = append(tiers, loaded.Skills)
			diagnostics = append(diagnostics, loaded.Diagnostics...)
		}
	}

	tiers = append(tiers, bundled)

	seen := make(map[string]bool)
	var merged []*Skill
	for _, tier := range tiers {
		for _, s := range tier {
			name := strings.ToLower(strings.TrimSpace(s.Name))
			if seen[name] {
				continue
			}
			seen[name] = true
			merged = append(merged, s)
		}
	}
	return Discovery{Skills: merged, Diagnostics: diagnostics}, nil
}

func readDirBounded(dir string, limit int) ([]os.DirEntry, bool, error) {
	file, err := os.Open(dir)
	if err != nil {
		return nil, false, err
	}
	entries, readErr := file.ReadDir(limit + 1)
	closeErr := file.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, false, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	if len(entries) > limit {
		return entries[:limit], true, nil
	}
	return entries, false, nil
}
