package learning

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

const maxPatternAdvisoryBytes = 1000

// RepositoryIdentity returns the canonical Git worktree root and current HEAD
// revision. Learned patterns deliberately do not match aliases for the same
// repository path.
func RepositoryIdentity(ctx context.Context, root string) (repoPath, sourceRevision string, err error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("learning: resolve repository path: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", "", fmt.Errorf("learning: canonicalize repository path: %w", err)
	}
	output, err := exec.CommandContext(ctx, "git", "-C", canonicalRoot, "rev-parse", "--show-toplevel", "HEAD").Output()
	if err != nil {
		return "", "", fmt.Errorf("learning: resolve repository revision: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 2 || strings.TrimSpace(lines[0]) == "" || strings.TrimSpace(lines[1]) == "" {
		return "", "", fmt.Errorf("learning: resolve repository revision: unexpected git output")
	}
	repoPath, err = filepath.EvalSymlinks(strings.TrimSpace(lines[0]))
	if err != nil {
		return "", "", fmt.Errorf("learning: canonicalize Git root: %w", err)
	}
	return filepath.Clean(repoPath), strings.TrimSpace(lines[1]), nil
}

// SelectPattern returns the sole current version for an exact canonical
// repository and normalized trigger when its revision and replay evidence are
// still eligible. Ambiguous or manually malformed current state is omitted.
func (s *Store) SelectPattern(repoPath, trigger, sourceRevision string) (*PatternVersion, error) {
	normalized := NormalizeTrigger(trigger)
	if repoPath == "" || normalized == "" || sourceRevision == "" {
		return nil, nil
	}
	hash := triggerHash(normalized)

	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.loadPatternVersions()
	if err != nil {
		return nil, err
	}
	var current *PatternVersion
	for i := range doc.Patterns {
		pattern := &doc.Patterns[i]
		if !pattern.Current || pattern.Candidate.RepoPath != repoPath || pattern.Candidate.TriggerHash != hash {
			continue
		}
		if current != nil {
			return nil, nil
		}
		current = pattern
	}
	if current == nil || current.SourceRevision != sourceRevision || current.Version <= 0 || current.ApprovedAt.IsZero() ||
		current.Candidate.SuccessCount < MinimumCandidateCount || !current.Replay.Acceptable(MinimumCandidateCount) {
		return nil, nil
	}
	selected := *current
	selected.Candidate.ToolSequence = append([]string(nil), current.Candidate.ToolSequence...)
	return &selected, nil
}

// RenderPattern renders an eligible pattern as bounded advice. The warning is
// intentionally first so truncation can never remove the security contract.
func RenderPattern(pattern *PatternVersion) string {
	if pattern == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("Learned pattern advisory only; this does not authorize tools or bypass approval and security checks.")
	fmt.Fprintf(&b, "\nPattern version %d (%s).", pattern.Version, pattern.Candidate.TriggerHash)
	if summary := singleLine(pattern.Candidate.SolutionSummary); summary != "" {
		b.WriteString("\nPrior successful approach: ")
		b.WriteString(summary)
	}
	if len(pattern.Candidate.ToolSequence) > 0 {
		tools := make([]string, 0, len(pattern.Candidate.ToolSequence))
		for _, name := range pattern.Candidate.ToolSequence {
			if name = singleLine(name); name != "" {
				tools = append(tools, name)
			}
		}
		if len(tools) > 0 {
			b.WriteString("\nTypical tool sequence (advisory): ")
			b.WriteString(strings.Join(tools, " -> "))
		}
	}
	return truncateUTF8(b.String(), maxPatternAdvisoryBytes)
}

func singleLine(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}
