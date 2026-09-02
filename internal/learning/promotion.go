package learning

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	maxSkillNameBytes         = 64
	maxSkillInstructionsBytes = 8 * 1024
	maxSkillTools             = 16
	maxSkillToolBytes         = 64
	maxSkillFileBytes         = 16 * 1024
)

// ErrSkillExists is returned when promotion would replace an existing skill.
var ErrSkillExists = errors.New("learning: skill already exists")

type promotedSkillFrontmatter struct {
	Name          string   `yaml:"name"`
	Description   string   `yaml:"description"`
	Version       string   `yaml:"version"`
	Triggers      []string `yaml:"triggers"`
	ToolsRequired []string `yaml:"tools_required,omitempty"`
}

// PromoteCandidate writes candidate as a user skill below skillsDir. It never
// replaces an existing skill directory.
func PromoteCandidate(skillsDir, name string, candidate PatternCandidate) (string, error) {
	safeName, err := safeSkillName(name)
	if err != nil {
		return "", err
	}
	data, err := renderCandidateSkill(safeName, candidate)
	if err != nil {
		return "", err
	}

	root, err := filepath.Abs(skillsDir)
	if err != nil {
		return "", fmt.Errorf("learning: resolve skills directory: %w", err)
	}
	if strings.TrimSpace(skillsDir) == "" {
		return "", fmt.Errorf("learning: skills directory is empty")
	}
	destination := filepath.Join(root, safeName)
	rel, err := filepath.Rel(root, destination)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("learning: skill destination escapes skills directory")
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("learning: create skills directory: %w", err)
	}
	if err := os.Mkdir(destination, 0o755); err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("%w: %s", ErrSkillExists, safeName)
		}
		return "", fmt.Errorf("learning: create skill directory %q: %w", destination, err)
	}

	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(destination)
		}
	}()

	tmp, err := os.CreateTemp(destination, ".SKILL.md-*")
	if err != nil {
		return "", fmt.Errorf("learning: create temporary skill file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("learning: set skill file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("learning: write temporary skill file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("learning: sync temporary skill file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("learning: close temporary skill file: %w", err)
	}

	path := filepath.Join(destination, "SKILL.md")
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("learning: publish skill file: %w", err)
	}
	committed = true
	return path, nil
}

func safeSkillName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("learning: unsafe skill name %q", name)
	}

	var b strings.Builder
	separator := false
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteByte(byte(r + ('a' - 'A')))
			separator = false
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			separator = false
		case r == '-' || r == '_' || r == ' ' || r == '\t':
			if b.Len() > 0 {
				separator = true
			}
		default:
			return "", fmt.Errorf("learning: unsafe skill name %q", name)
		}
		if separator && b.Len() > 0 && b.String()[b.Len()-1] != '-' {
			b.WriteByte('-')
		}
	}
	safe := strings.Trim(b.String(), "-")
	if safe == "" || len(safe) > maxSkillNameBytes {
		return "", fmt.Errorf("learning: unsafe skill name %q", name)
	}
	return safe, nil
}

func renderCandidateSkill(name string, candidate PatternCandidate) ([]byte, error) {
	tools := make([]string, 0, min(len(candidate.ToolSequence), maxSkillTools))
	for _, tool := range candidate.ToolSequence {
		tool = truncateUTF8(strings.TrimSpace(tool), maxSkillToolBytes)
		if tool != "" {
			tools = append(tools, tool)
		}
		if len(tools) == maxSkillTools {
			break
		}
	}

	meta, err := yaml.Marshal(promotedSkillFrontmatter{
		Name:          name,
		Description:   "A workflow learned from repeated successful sessions.",
		Version:       "1.0.0",
		Triggers:      strings.Split(strings.ReplaceAll(name, "-", " "), " "),
		ToolsRequired: tools,
	})
	if err != nil {
		return nil, fmt.Errorf("learning: render skill frontmatter: %w", err)
	}

	instructions := truncateUTF8(strings.TrimSpace(candidate.SolutionSummary), maxSkillInstructionsBytes)
	if instructions == "" {
		instructions = "Follow the successful workflow represented by this candidate."
	}
	var body strings.Builder
	fmt.Fprintf(&body, "---\n%s---\n# %s\n\n## Instructions\n\n%s\n", meta, strings.ReplaceAll(name, "-", " "), instructions)
	if len(tools) > 0 {
		body.WriteString("\n## Typical Tool Sequence\n")
		for i, tool := range tools {
			fmt.Fprintf(&body, "\n%d. %s", i+1, tool)
		}
		body.WriteByte('\n')
	}
	data := []byte(body.String())
	if len(data) > maxSkillFileBytes {
		return nil, fmt.Errorf("learning: rendered skill exceeds %d bytes", maxSkillFileBytes)
	}
	return data, nil
}

func truncateUTF8(value string, limit int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return strings.TrimSpace(value[:limit])
}
