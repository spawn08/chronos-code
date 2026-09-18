package eval

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const TaskManifestVersion = "chronos.eval.tasks.v1"

type FixtureSpec struct {
	Source   string `yaml:"source"`
	Revision string `yaml:"revision"`
	Type     string `yaml:"type"`
}

type PromptSpec struct {
	Reference string `yaml:"reference"`
}

type HiddenCommand struct {
	ID      string   `yaml:"id"`
	Command []string `yaml:"command"`
}

type ArtifactAssertion struct {
	Path     string `yaml:"path"`
	Exists   *bool  `yaml:"exists,omitempty"`
	Contains string `yaml:"contains,omitempty"`
}

type HiddenGraderSpec struct {
	Commands  []HiddenCommand     `yaml:"commands"`
	Artifacts []ArtifactAssertion `yaml:"artifacts"`
}

// Ablation selects only features implemented by Chronos Code itself.
type Ablation struct {
	Graph       bool `yaml:"graph" json:"graph"`
	Skills      bool `yaml:"skills" json:"skills"`
	Specialists bool `yaml:"specialists" json:"specialists"`
	Repair      bool `yaml:"repair" json:"repair"`
	Worktrees   bool `yaml:"worktrees" json:"worktrees"`
}

type ManifestTask struct {
	ID          string           `yaml:"id"`
	Kind        string           `yaml:"kind"`
	Fixture     FixtureSpec      `yaml:"fixture"`
	Prompt      PromptSpec       `yaml:"prompt"`
	Language    string           `yaml:"language"`
	Timeout     string           `yaml:"timeout"`
	Permissions []string         `yaml:"permissions"`
	Grader      HiddenGraderSpec `yaml:"hidden_grader"`
	Ablation    Ablation         `yaml:"ablation"`
	Expected    string           `yaml:"expected_outcome,omitempty"`
	promptText  string
}

type TaskManifest struct {
	Version string         `yaml:"version"`
	Tasks   []ManifestTask `yaml:"tasks"`
}

// LoadTaskManifest strictly decodes and resolves a versioned task manifest.
func LoadTaskManifest(path string) (TaskManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TaskManifest{}, fmt.Errorf("eval: read task manifest: %w", err)
	}
	var manifest TaskManifest
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return TaskManifest{}, fmt.Errorf("eval: decode task manifest: %w", err)
	}
	if manifest.Version != TaskManifestVersion {
		return TaskManifest{}, fmt.Errorf("eval: unsupported task manifest version %q", manifest.Version)
	}
	if len(manifest.Tasks) == 0 {
		return TaskManifest{}, fmt.Errorf("eval: task manifest is empty")
	}
	base := filepath.Dir(path)
	seen := make(map[string]struct{}, len(manifest.Tasks))
	for i := range manifest.Tasks {
		task := &manifest.Tasks[i]
		if err := validateManifestTask(task, base, seen); err != nil {
			return TaskManifest{}, fmt.Errorf("eval: task %d: %w", i+1, err)
		}
	}
	return manifest, nil
}

func validateManifestTask(task *ManifestTask, base string, seen map[string]struct{}) error {
	if task.ID == "" || task.Kind == "" || task.Language == "" {
		return fmt.Errorf("id, kind, and language are required")
	}
	if _, exists := seen[task.ID]; exists {
		return fmt.Errorf("duplicate task id %q", task.ID)
	}
	seen[task.ID] = struct{}{}
	if task.Fixture.Source == "" || task.Fixture.Revision == "" {
		return fmt.Errorf("task %s: fixture source and revision are required", task.ID)
	}
	if strings.TrimSpace(task.Fixture.Source) != task.Fixture.Source || strings.TrimSpace(task.Fixture.Revision) != task.Fixture.Revision {
		return fmt.Errorf("task %s: fixture source and revision must not contain surrounding whitespace", task.ID)
	}
	if task.Fixture.Type != "git" && task.Fixture.Type != "local" {
		return fmt.Errorf("task %s: fixture type must be git or local", task.ID)
	}
	if task.Prompt.Reference == "" {
		return fmt.Errorf("task %s: prompt reference is required", task.ID)
	}
	timeout, err := time.ParseDuration(task.Timeout)
	if err != nil {
		return fmt.Errorf("task %s: timeout must be a duration: %w", task.ID, err)
	}
	if timeout <= 0 {
		return fmt.Errorf("task %s: timeout must be positive", task.ID)
	}
	if len(task.Permissions) == 0 {
		return fmt.Errorf("task %s: permissions are required", task.ID)
	}
	allowed := map[string]bool{"read": true, "write": true, "shell": true, "network": true}
	permissions := make(map[string]struct{}, len(task.Permissions))
	for _, permission := range task.Permissions {
		if !allowed[permission] {
			return fmt.Errorf("task %s: unsupported permission %q", task.ID, permission)
		}
		if _, exists := permissions[permission]; exists {
			return fmt.Errorf("task %s: duplicate permission %q", task.ID, permission)
		}
		permissions[permission] = struct{}{}
	}
	if len(task.Grader.Commands) == 0 && len(task.Grader.Artifacts) == 0 {
		return fmt.Errorf("task %s: hidden grader requires a command or artifact assertion", task.ID)
	}
	commandIDs := make(map[string]struct{}, len(task.Grader.Commands))
	for _, command := range task.Grader.Commands {
		if command.ID == "" || len(command.Command) == 0 || command.Command[0] == "" {
			return fmt.Errorf("task %s: hidden grader command id and argv are required", task.ID)
		}
		if _, exists := commandIDs[command.ID]; exists {
			return fmt.Errorf("task %s: duplicate hidden grader command %q", task.ID, command.ID)
		}
		commandIDs[command.ID] = struct{}{}
	}
	for _, assertion := range task.Grader.Artifacts {
		if !safeRelativePath(assertion.Path) || assertion.Exists == nil && assertion.Contains == "" {
			return fmt.Errorf("task %s: invalid artifact assertion for %q", task.ID, assertion.Path)
		}
		if assertion.Exists != nil && !*assertion.Exists && assertion.Contains != "" {
			return fmt.Errorf("task %s: absent artifact %q cannot require content", task.ID, assertion.Path)
		}
	}
	if task.Expected != "" && task.Expected != "passed" && task.Expected != "failed" {
		return fmt.Errorf("task %s: expected_outcome must be passed or failed", task.ID)
	}
	if task.Fixture.Type == "local" {
		task.Fixture.Source = resolveReference(base, task.Fixture.Source)
		info, err := os.Stat(task.Fixture.Source)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("task %s: local fixture source is not a directory", task.ID)
		}
	} else if strings.HasPrefix(task.Fixture.Source, ".") {
		task.Fixture.Source = resolveReference(base, task.Fixture.Source)
	}
	task.Prompt.Reference = resolveReference(base, task.Prompt.Reference)
	prompt, err := os.ReadFile(task.Prompt.Reference)
	if err != nil {
		return fmt.Errorf("task %s: read prompt reference: %w", task.ID, err)
	}
	if strings.TrimSpace(string(prompt)) == "" {
		return fmt.Errorf("task %s: prompt reference is empty", task.ID)
	}
	task.promptText = string(prompt)
	return nil
}

func resolveReference(base, reference string) string {
	if filepath.IsAbs(reference) {
		return filepath.Clean(reference)
	}
	return filepath.Clean(filepath.Join(base, reference))
}

func safeRelativePath(path string) bool {
	clean := filepath.Clean(path)
	return path != "" && !filepath.IsAbs(path) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func (t ManifestTask) TaskRun(model ModelSettings, runID string) (TaskRun, error) {
	timeout, err := time.ParseDuration(t.Timeout)
	if err != nil {
		return TaskRun{}, fmt.Errorf("eval: task %s timeout: %w", t.ID, err)
	}
	return TaskRun{
		TaskID: t.ID, RunID: runID, Fixture: t.Fixture.Source, FixtureType: t.Fixture.Type,
		Revision: t.Fixture.Revision, Model: model, Timeout: timeout, Prompt: t.promptText,
		Permissions: append([]string(nil), t.Permissions...), Grading: t.Grader, Ablation: t.Ablation,
	}, nil
}
