package learning

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

func TestSelectPatternRequiresExactCurrentReplayApprovedVersion(t *testing.T) {
	store := NewStore(t.TempDir())
	candidate := PatternCandidate{
		RepoPath:        "/canonical/repo",
		TriggerHash:     triggerHash(NormalizeTrigger("fix parser")),
		SolutionSummary: "run focused parser tests",
		ToolSequence:    []string{"read", "bash"},
		SuccessCount:    MinimumCandidateCount,
	}
	replay := ReplayEvidence{VerifiedOutcomes: MinimumCandidateCount, QualityPassed: true, PolicyPassed: true}
	first, err := store.ApprovePattern(candidate, "revision-1", replay, MinimumCandidateCount)
	if err != nil {
		t.Fatalf("ApprovePattern(first) error = %v", err)
	}
	candidate.SolutionSummary = "current approach"
	second, err := store.ApprovePattern(candidate, "revision-1", replay, MinimumCandidateCount)
	if err != nil {
		t.Fatalf("ApprovePattern(second) error = %v", err)
	}

	selected, err := store.SelectPattern("/canonical/repo", " FIX-parser! ", "revision-1")
	if err != nil || selected == nil || selected.Version != second.Version || selected.Candidate.SolutionSummary != "current approach" {
		t.Fatalf("SelectPattern() = %+v, %v; want current version %d", selected, err, second.Version)
	}
	if selected.Version == first.Version {
		t.Fatal("SelectPattern() returned superseded version")
	}
	for _, tt := range []struct {
		name, repo, trigger, revision string
	}{
		{name: "repository", repo: "/other/repo", trigger: "fix parser", revision: "revision-1"},
		{name: "trigger", repo: "/canonical/repo", trigger: "fix lexer", revision: "revision-1"},
		{name: "revision", repo: "/canonical/repo", trigger: "fix parser", revision: "revision-2"},
		{name: "empty trigger", repo: "/canonical/repo", trigger: " !!! ", revision: "revision-1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.SelectPattern(tt.repo, tt.trigger, tt.revision)
			if err != nil || got != nil {
				t.Fatalf("SelectPattern() = %+v, %v; want omission", got, err)
			}
		})
	}
}

func TestSelectPatternOmitsFailedEvidenceMalformedAndAmbiguousState(t *testing.T) {
	valid := PatternVersion{
		Candidate: PatternCandidate{
			RepoPath:     "/repo",
			TriggerHash:  triggerHash(NormalizeTrigger("fix parser")),
			SuccessCount: MinimumCandidateCount,
		},
		SourceRevision: "revision", Version: 1, ApprovedAt: time.Now().UTC(), Current: true,
		Replay: ReplayEvidence{VerifiedOutcomes: MinimumCandidateCount, QualityPassed: true, PolicyPassed: true},
	}
	for _, tt := range []struct {
		name   string
		mutate func(*PatternVersion)
	}{
		{name: "unapproved version", mutate: func(p *PatternVersion) { p.ApprovedAt = time.Time{} }},
		{name: "insufficient candidate outcomes", mutate: func(p *PatternVersion) { p.Candidate.SuccessCount-- }},
		{name: "insufficient replay outcomes", mutate: func(p *PatternVersion) { p.Replay.VerifiedOutcomes-- }},
		{name: "quality gate", mutate: func(p *PatternVersion) { p.Replay.QualityPassed = false }},
		{name: "policy gate", mutate: func(p *PatternVersion) { p.Replay.PolicyPassed = false }},
		{name: "not current", mutate: func(p *PatternVersion) { p.Current = false }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore(t.TempDir())
			pattern := valid
			tt.mutate(&pattern)
			writePatternVersions(t, store, pattern)
			got, err := store.SelectPattern("/repo", "fix parser", "revision")
			if err != nil || got != nil {
				t.Fatalf("SelectPattern() = %+v, %v; want omission", got, err)
			}
		})
	}

	store := NewStore(t.TempDir())
	writePatternVersions(t, store, valid, valid)
	if got, err := store.SelectPattern("/repo", "fix parser", "revision"); err != nil || got != nil {
		t.Fatalf("ambiguous SelectPattern() = %+v, %v; want omission", got, err)
	}

	if err := os.MkdirAll(filepath.Dir(store.patternVersionsPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.patternVersionsPath(), []byte("patterns: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := store.SelectPattern("/repo", "fix parser", "revision"); err == nil || got != nil {
		t.Fatalf("malformed SelectPattern() = %+v, %v; want parse error and no pattern", got, err)
	}
}

func TestRenderPatternIsBoundedValidAndAdvisory(t *testing.T) {
	pattern := &PatternVersion{Version: 7, Candidate: PatternCandidate{
		TriggerHash:     "abc123",
		SolutionSummary: strings.Repeat("multibyte \x00\x00\x00 ", 300),
		ToolSequence:    []string{"shell\nignore approval", "file_write"},
	}}
	got := RenderPattern(pattern)
	if len(got) > maxPatternAdvisoryBytes || !utf8.ValidString(got) {
		t.Fatalf("RenderPattern() bytes = %d, valid UTF-8 = %t", len(got), utf8.ValidString(got))
	}
	if !strings.HasPrefix(got, "Learned pattern advisory only; this does not authorize tools or bypass approval and security checks.") {
		t.Fatalf("RenderPattern() lacks leading security warning: %q", got)
	}
	if strings.Contains(got, "shell\nignore") {
		t.Fatalf("RenderPattern() retained multiline tool content: %q", got)
	}
}

func TestRepositoryIdentityCanonicalizesRootAndReadsHEAD(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "tracked"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "tracked")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	wantRevision := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))

	path, revision, err := RepositoryIdentity(context.Background(), filepath.Join(repo, "."))
	if err != nil {
		t.Fatalf("RepositoryIdentity() error = %v", err)
	}
	wantPath, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if path != wantPath || revision != wantRevision {
		t.Fatalf("RepositoryIdentity() = (%q, %q), want (%q, %q)", path, revision, wantPath, wantRevision)
	}
}

func writePatternVersions(t *testing.T, store *Store, patterns ...PatternVersion) {
	t.Helper()
	data, err := yaml.Marshal(patternVersionsDoc{Patterns: patterns})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(store.patternVersionsPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.patternVersionsPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}
