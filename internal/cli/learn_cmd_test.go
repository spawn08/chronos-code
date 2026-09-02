package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/learning"
	"github.com/spawn08/chronos-code/internal/skills"
)

func TestLearnCandidatesEmptyAndDeterministic(t *testing.T) {
	workspace, _, configFile := setupLearnCommandTest(t)

	got, err := runLearnForTest(t, configFile, "candidates")
	if err != nil {
		t.Fatalf("runLearn(candidates) empty error = %v", err)
	}
	if got != "no learning candidates\n" {
		t.Fatalf("runLearn(candidates) empty output = %q", got)
	}

	ctx := context.Background()
	store, err := learning.OpenSQLStore(ctx, filepath.Join(workspace, config.ConfigDirName, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCandidates(ctx, []learning.PatternCandidate{
		{RepoPath: workspace, TriggerHash: "bbbb", ToolSequence: []string{"write"}, SuccessCount: 4, FailCount: 1},
		{RepoPath: workspace, TriggerHash: "aaaa", ToolSequence: []string{"read", "test"}, SuccessCount: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(ctx); err != nil {
		t.Fatal(err)
	}

	want := "aaaa  success 3  failure 0  tools read,test\n" +
		"bbbb  success 4  failure 1  tools write\n"
	for range 2 {
		got, err = runLearnForTest(t, configFile, "candidates")
		if err != nil {
			t.Fatalf("runLearn(candidates) error = %v", err)
		}
		if got != want {
			t.Fatalf("runLearn(candidates) output = %q, want %q", got, want)
		}
	}
}

func TestLearnPromoteWritesConfiguredUserSkill(t *testing.T) {
	workspace, skillsDir, configFile := setupLearnCommandTest(t)
	seedLearnCandidate(t, workspace, learning.PatternCandidate{
		RepoPath: workspace, TriggerHash: "abc123", SolutionSummary: "Run focused tests first.", ToolSequence: []string{"read", "bash"}, SuccessCount: 3,
	})

	got, err := runLearnForTest(t, configFile, "promote", "abc123")
	if err != nil {
		t.Fatalf("runLearn(promote) error = %v", err)
	}
	wantPath := filepath.Join(skillsDir, "learned-abc123", "SKILL.md")
	if got != "promoted candidate \"abc123\" to "+wantPath+"\n" {
		t.Fatalf("runLearn(promote) output = %q", got)
	}
	loaded, err := skills.LoadDir(skillsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Name != "learned-abc123" || !strings.Contains(loaded[0].Body, "Run focused tests first.") {
		t.Fatalf("promoted skills = %+v", loaded)
	}
}

func TestLearnPromoteUnknownAndExistingDoNotMutate(t *testing.T) {
	workspace, skillsDir, configFile := setupLearnCommandTest(t)
	seedLearnCandidate(t, workspace, learning.PatternCandidate{
		RepoPath: workspace, TriggerHash: "abc123", SolutionSummary: "original", SuccessCount: 3,
	})

	if _, err := runLearnForTest(t, configFile, "promote", "missing"); err == nil || !strings.Contains(err.Error(), `learning candidate "missing" not found`) {
		t.Fatalf("runLearn(promote missing) error = %v", err)
	}
	if _, err := os.Stat(skillsDir); !os.IsNotExist(err) {
		t.Fatalf("unknown promotion mutated skills directory: %v", err)
	}

	if _, err := runLearnForTest(t, configFile, "promote", "abc123"); err != nil {
		t.Fatalf("first runLearn(promote) error = %v", err)
	}
	path := filepath.Join(skillsDir, "learned-abc123", "SKILL.md")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runLearnForTest(t, configFile, "promote", "abc123"); err == nil || !strings.Contains(err.Error(), "skill already exists") {
		t.Fatalf("second runLearn(promote) error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("existing promoted skill was modified")
	}
}

func setupLearnCommandTest(t *testing.T) (workspace, skillsDir, configFile string) {
	t.Helper()
	workspace = t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(workspace)
	skillsDir = filepath.Join(home, "configured-skills")
	configDir := filepath.Join(workspace, config.ConfigDirName)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configFile = filepath.Join(configDir, "config.yaml")
	data := []byte("workspace:\n  root: " + workspace + "\nskills_dir: " + skillsDir + "\n")
	if err := os.WriteFile(configFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return workspace, skillsDir, configFile
}

func seedLearnCandidate(t *testing.T, workspace string, candidate learning.PatternCandidate) {
	t.Helper()
	ctx := context.Background()
	store, err := learning.OpenSQLStore(ctx, filepath.Join(workspace, config.ConfigDirName, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCandidates(ctx, []learning.PatternCandidate{candidate}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func runLearnForTest(t *testing.T, path string, args ...string) (string, error) {
	t.Helper()
	resetGlobalFlags(t, append([]string{"chronos-code", "learn"}, args...))
	configPath = path

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = w
	err = runLearn()
	os.Stdout = originalStdout
	if closeErr := w.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	output, readErr := io.ReadAll(r)
	if closeErr := r.Close(); readErr == nil && closeErr != nil {
		readErr = closeErr
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(output), err
}
