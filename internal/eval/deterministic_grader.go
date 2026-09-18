package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type hiddenGrade struct {
	Passed   bool
	Evidence []VerificationEvidence
	Failures []string
}

// runHiddenGrader is deliberately called only after agent execution returns.
func runHiddenGrader(ctx context.Context, workspace string, environment []string, spec HiddenGraderSpec) hiddenGrade {
	grade := hiddenGrade{Passed: true}
	for _, check := range spec.Commands {
		started := time.Now().UTC()
		command := exec.CommandContext(ctx, check.Command[0], check.Command[1:]...)
		command.Dir = workspace
		command.Env = append(os.Environ(), environment...)
		output, err := command.CombinedOutput()
		passed := err == nil
		grade.Evidence = append(grade.Evidence, VerificationEvidence{
			ID: check.ID, Command: strings.Join(check.Command, " "), Passed: passed, ExecutedAt: started,
		})
		if !passed {
			grade.Passed = false
			message := strings.TrimSpace(string(output))
			if len(message) > maxEvidenceOutput {
				message = message[:maxEvidenceOutput]
			}
			grade.Failures = append(grade.Failures, fmt.Sprintf("%s failed: %s", check.ID, message))
		}
	}
	for _, assertion := range spec.Artifacts {
		path := filepath.Join(workspace, filepath.Clean(assertion.Path))
		info, statErr := os.Stat(path)
		exists := statErr == nil
		passed := true
		if exists {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || !withinWorkspace(workspace, resolved) {
				passed = false
			}
		}
		if assertion.Exists != nil && exists != *assertion.Exists {
			passed = false
		}
		if assertion.Contains != "" {
			data, err := os.ReadFile(path)
			if err != nil || info.IsDir() || !strings.Contains(string(data), assertion.Contains) {
				passed = false
			}
		}
		if !passed {
			grade.Passed = false
			grade.Failures = append(grade.Failures, fmt.Sprintf("artifact assertion failed: %s", assertion.Path))
		}
	}
	return grade
}

func withinWorkspace(workspace, path string) bool {
	if canonical, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = canonical
	}
	relative, err := filepath.Rel(workspace, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
