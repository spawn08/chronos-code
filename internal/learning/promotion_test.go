package learning

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/skills"
)

func TestPromoteCandidateRoundTripsThroughSkillsLoader(t *testing.T) {
	root := t.TempDir()
	candidate := PatternCandidate{
		SolutionSummary: "Run the focused test first, then run the package suite.",
		ToolSequence:    []string{"read", "bash"},
	}

	path, err := PromoteCandidate(root, "Go_Test Workflow", candidate)
	if err != nil {
		t.Fatalf("PromoteCandidate() error = %v", err)
	}
	wantPath := filepath.Join(root, "go-test-workflow", "SKILL.md")
	if path != wantPath {
		t.Fatalf("PromoteCandidate() path = %q, want %q", path, wantPath)
	}

	loaded, err := skills.LoadDir(root)
	if err != nil {
		t.Fatalf("skills.LoadDir() error = %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("skills.LoadDir() returned %d skills, want 1", len(loaded))
	}
	if loaded[0].Name != "go-test-workflow" || loaded[0].Version != "1.0.0" {
		t.Errorf("loaded skill = %+v", loaded[0])
	}
	if loaded[0].Body != "# go test workflow\n\n## Instructions\n\nRun the focused test first, then run the package suite.\n\n## Typical Tool Sequence\n\n1. read\n2. bash" {
		t.Errorf("loaded skill body = %q", loaded[0].Body)
	}
	if got := loaded[0].ToolsRequired; len(got) != 2 || got[0] != "read" || got[1] != "bash" {
		t.Errorf("loaded tools = %v, want [read bash]", got)
	}
}

func TestPromoteCandidateRejectsUnsafeNamesAndEscapes(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "escaped-skill")
	unsafe := []string{"", ".", "..", "../escaped-skill", "nested/skill", `nested\skill`, "/absolute", "bad:name", strings.Repeat("a", maxSkillNameBytes+1)}
	for _, name := range unsafe {
		t.Run(name, func(t *testing.T) {
			if _, err := PromoteCandidate(root, name, PatternCandidate{}); err == nil {
				t.Fatalf("PromoteCandidate(%q) succeeded, want error", name)
			}
		})
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside destination exists after rejected traversal: %v", err)
	}
}

func TestPromoteCandidateDoesNotOverwriteExistingSkill(t *testing.T) {
	root := t.TempDir()
	path, err := PromoteCandidate(root, "existing", PatternCandidate{SolutionSummary: "original"})
	if err != nil {
		t.Fatalf("first PromoteCandidate() error = %v", err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = PromoteCandidate(root, "existing", PatternCandidate{SolutionSummary: "replacement"})
	if !errors.Is(err, ErrSkillExists) {
		t.Fatalf("second PromoteCandidate() error = %v, want ErrSkillExists", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("existing SKILL.md was modified")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "SKILL.md" {
		t.Fatalf("skill directory entries = %v, want only SKILL.md", entries)
	}
}

func TestSkillWriterOutputIsDeterministicAndBounded(t *testing.T) {
	candidate := PatternCandidate{
		SolutionSummary: strings.Repeat("instruction ", maxSkillInstructionsBytes),
		ToolSequence:    make([]string, maxSkillTools+10),
	}
	for i := range candidate.ToolSequence {
		candidate.ToolSequence[i] = strings.Repeat("tool", maxSkillToolBytes)
	}

	var outputs [][]byte
	for range 2 {
		root := t.TempDir()
		path, err := PromoteCandidate(root, "Deterministic Skill", candidate)
		if err != nil {
			t.Fatalf("PromoteCandidate() error = %v", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > maxSkillFileBytes {
			t.Fatalf("SKILL.md size = %d, want at most %d", len(data), maxSkillFileBytes)
		}
		outputs = append(outputs, data)
	}
	if string(outputs[0]) != string(outputs[1]) {
		t.Fatal("identical candidate rendered different SKILL.md output")
	}
}

func TestPromoteCandidateFailureLeavesNoPartialSkill(t *testing.T) {
	root := t.TempDir()
	blockingFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := PromoteCandidate(filepath.Join(blockingFile, "skills"), "partial", PatternCandidate{}); err == nil {
		t.Fatal("PromoteCandidate() succeeded, want error")
	}
	if _, err := os.Stat(filepath.Join(blockingFile, "skills", "partial")); err == nil {
		t.Fatal("failed promotion left a partial skill directory")
	}
}
