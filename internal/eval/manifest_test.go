package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTaskManifestResolvesAgentInputAndKeepsGraderSeparate(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture")
	if err := os.Mkdir(fixture, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("fix it\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "tasks.yaml")
	manifest := `version: chronos.eval.tasks.v1
tasks:
  - id: task-1
    kind: bugfix
    fixture: {type: local, source: fixture, revision: fixture-v1}
    prompt: {reference: prompt.md}
    language: go
    timeout: 1m
    permissions: [read, write]
    hidden_grader:
      commands: [{id: hidden, command: [go, test, ./...]}]
      artifacts: [{path: result.go, exists: true}]
    ablation: {graph: true, skills: false, specialists: true, repair: false, worktrees: true}
`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadTaskManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadTaskManifest: %v", err)
	}
	run, err := loaded.Tasks[0].TaskRun(ModelSettings{Provider: "test", Name: "model"}, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Prompt != "fix it\n" || len(run.Grading.Commands) != 1 || !run.Ablation.Graph || run.Ablation.Skills {
		t.Fatalf("run = %+v", run)
	}
	execution := TaskExecution{Instructions: run.Prompt, Permissions: run.Permissions}
	if strings.Contains(execution.Instructions, "go test") {
		t.Fatalf("agent instructions leaked hidden grader: %q", execution.Instructions)
	}
}

func TestLoadTaskManifestRejectsUnsafeOrIncompleteTasks(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("task"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := `version: chronos.eval.tasks.v1
tasks:
  - id: task-1
    kind: bugfix
    fixture: {type: local, source: fixture, revision: fixture-v1}
    prompt: {reference: prompt.md}
    language: go
    timeout: 1m
    permissions: [read]
    hidden_grader:
      artifacts: [{path: ../outside, exists: true}]
    ablation: {}
`
	path := filepath.Join(dir, "tasks.yaml")
	if err := os.WriteFile(path, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTaskManifest(path); err == nil || !strings.Contains(err.Error(), "invalid artifact assertion") {
		t.Fatalf("LoadTaskManifest error = %v", err)
	}
}

func TestCheckedInGoCorpusCoversRequiredTaskKinds(t *testing.T) {
	manifest, err := LoadTaskManifest(filepath.Join("..", "..", "benchmark", "tasks", "manifest-v1.yaml"))
	if err != nil {
		t.Fatalf("LoadTaskManifest: %v", err)
	}
	kinds := make(map[string]bool)
	for _, task := range manifest.Tasks {
		kinds[task.Kind] = true
		if task.Language != "go" || len(task.Grader.Commands) == 0 || len(task.Grader.Artifacts) == 0 {
			t.Fatalf("task lacks deterministic Go fixture assertions: %+v", task)
		}
	}
	for _, kind := range []string{"bugfix", "multi_file_feature", "refactor", "test_generation"} {
		if !kinds[kind] {
			t.Errorf("checked-in corpus is missing %s", kind)
		}
	}
}
