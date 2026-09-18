package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/eval"
)

func TestRunEvalPPDRequiresValidateOnly(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code", "eval", "ppd"})
	err := runEval()
	if err == nil || !strings.Contains(err.Error(), "eval ppd --validate-only") {
		t.Fatalf("runEval() error = %v, want PPD usage", err)
	}
}

func TestRunEvalTasksValidatesCheckedInManifest(t *testing.T) {
	var output bytes.Buffer
	err := runEvalTasksTo([]string{
		"--validate-only", "--manifest", filepath.Join("..", "..", "benchmark", "tasks", "manifest-v1.yaml"),
	}, &output)
	if err != nil {
		t.Fatalf("runEvalTasksTo: %v", err)
	}
	if !strings.Contains(output.String(), "validated 4 tasks") || !strings.Contains(output.String(), eval.TaskManifestVersion) {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunEvalPPDReportRejectsUnknownFlag(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code", "eval", "ppd", "--report", "--basline", "missing.json"})
	err := runEval()
	if err == nil || !strings.Contains(err.Error(), "invalid PPD report flag") {
		t.Fatalf("runEval() error = %v, want invalid report flag", err)
	}
}
