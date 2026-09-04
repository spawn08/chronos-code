package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos-code/internal/skills"
)

func runSkills() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: chronos-code skills [list|show <name>]")
	}
	switch os.Args[2] {
	case "list":
		return skillsList()
	case "show":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: chronos-code skills show <name>")
		}
		return skillsShow(os.Args[3])
	default:
		return fmt.Errorf("unknown skills command: %s", os.Args[2])
	}
}

func skillsList() error {
	catalog, err := discoverSkillCatalog()
	if err != nil {
		return err
	}
	if len(catalog) == 0 {
		fmt.Println("no skills discovered")
		return nil
	}
	fmt.Printf("skills (%d):\n", len(catalog))
	for _, s := range catalog {
		source := s.Source
		if source == "" {
			source = "unknown"
		}
		fmt.Printf("  %-24s %-50s [%s]\n", s.Name, s.Description, source)
	}
	return nil
}

func skillsShow(name string) error {
	catalog, err := discoverSkillCatalog()
	if err != nil {
		return err
	}
	for _, s := range catalog {
		if strings.EqualFold(s.Name, name) {
			fmt.Printf("Name:        %s\n", s.Name)
			fmt.Printf("Version:     %s\n", s.Version)
			fmt.Printf("Description: %s\n", s.Description)
			if s.Author != "" {
				fmt.Printf("Author:      %s\n", s.Author)
			}
			if len(s.Triggers) > 0 {
				fmt.Printf("Triggers:    %s\n", strings.Join(s.Triggers, ", "))
			}
			if s.ModelHint != "" {
				fmt.Printf("Model hint:  %s\n", s.ModelHint)
			}
			if len(s.ToolsRequired) > 0 {
				fmt.Printf("Tools:       %s\n", strings.Join(s.ToolsRequired, ", "))
			}
			fmt.Printf("Source:      %s\n", s.Source)
			fmt.Printf("\n--- Body ---\n%s\n", s.Body)
			return nil
		}
	}
	return fmt.Errorf("skill %q not found", name)
}

func discoverSkillCatalog() ([]*skills.Skill, error) {
	bundledData, err := defaults.ReadFile("skills/default-skills.yaml")
	if err != nil {
		return nil, fmt.Errorf("read bundled skills: %w", err)
	}
	bundled, err := skills.LoadBundledYAML(bundledData)
	if err != nil {
		return nil, fmt.Errorf("parse bundled skills: %w", err)
	}
	root := config.WorkspaceRoot()
	return skills.Discover(root, bundled)
}
