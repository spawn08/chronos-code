package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/defaults"
)

type ProjectType string

const (
	ProjectGo      ProjectType = "go"
	ProjectNode    ProjectType = "node"
	ProjectPython  ProjectType = "python"
	ProjectRust    ProjectType = "rust"
	ProjectJava    ProjectType = "java"
	ProjectUnknown ProjectType = "unknown"
)

func runInit() error {
	dir := config.ConfigDirName

	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		fmt.Printf("%s/ already exists. Overwrite? [y/N]: ", dir)
		var answer string
		fmt.Scanln(&answer)
		if !strings.HasPrefix(strings.ToLower(answer), "y") {
			fmt.Println("aborted")
			return nil
		}
	}

	projType := detectProjectType(".")
	fmt.Printf("Detected project type: %s\n", projType)

	if err := writeEmbeddedDefaults(dir); err != nil {
		return fmt.Errorf("write defaults: %w", err)
	}

	if err := customizeForProject(dir, projType); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not customize for %s: %v\n", projType, err)
	}

	fmt.Printf("\nInitialized %s/ with:\n", dir)
	fmt.Println("  config.yaml        — main config (model, storage, defaults)")
	fmt.Println("  security.yaml      — security policy (path/shell restrictions)")
	fmt.Println("  agents/            — Chronos Code primary plus specialist prompts")
	fmt.Println("  skills/            — 11 default skills")
	fmt.Println("  guardrails/        — guardrail presets")
	fmt.Printf("\nRun 'chronos-code' to start the REPL.\n")
	fmt.Printf("Set ANTHROPIC_API_KEY (or configure a different provider in config.yaml) first.\n")

	return nil
}

func writeEmbeddedDefaults(dir string) error {
	return fs.WalkDir(defaults.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, path)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := defaults.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func customizeForProject(dir string, projType ProjectType) error {
	configPath := filepath.Join(dir, "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return err
	}

	shellCommands := defaultShellCommands(projType)
	securityPath := filepath.Join(dir, "security.yaml")
	secData, err := os.ReadFile(securityPath)
	if err != nil {
		return err
	}
	var security map[string]any
	if err := yaml.Unmarshal(secData, &security); err != nil {
		return err
	}
	if shell, ok := security["shell"].(map[string]any); ok {
		shell["allowed_commands"] = shellCommands
	}
	out, err := yaml.Marshal(security)
	if err != nil {
		return err
	}
	return os.WriteFile(securityPath, out, 0o644)
}

func defaultShellCommands(pt ProjectType) []string {
	base := []string{"git", "ls", "cat", "grep", "rg", "find", "wc", "diff", "jq", "make"}
	switch pt {
	case ProjectGo:
		return append(base, "go", "go test", "go build", "go vet", "golangci-lint")
	case ProjectNode:
		return append(base, "npm", "node", "npx", "yarn", "pnpm", "tsc", "eslint")
	case ProjectPython:
		return append(base, "python", "python3", "pip", "pip3", "pytest", "ruff", "mypy")
	case ProjectRust:
		return append(base, "cargo", "rustc", "clippy")
	case ProjectJava:
		return append(base, "java", "javac", "mvn", "gradle")
	default:
		return base
	}
}

func detectProjectType(dir string) ProjectType {
	markers := map[string]ProjectType{
		"go.mod":         ProjectGo,
		"package.json":   ProjectNode,
		"Cargo.toml":     ProjectRust,
		"pyproject.toml": ProjectPython,
		"setup.py":       ProjectPython,
		"requirements.txt": ProjectPython,
		"pom.xml":        ProjectJava,
		"build.gradle":   ProjectJava,
		"build.gradle.kts": ProjectJava,
	}
	for file, pt := range markers {
		if _, err := os.Stat(filepath.Join(dir, file)); err == nil {
			return pt
		}
	}
	return ProjectUnknown
}
