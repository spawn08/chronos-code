package cli

import (
	"strings"
	"testing"
)

func TestRunEvalPPDRequiresValidateOnly(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code", "eval", "ppd"})
	err := runEval()
	if err == nil || !strings.Contains(err.Error(), "eval ppd --validate-only") {
		t.Fatalf("runEval() error = %v, want PPD usage", err)
	}
}

func TestRunEvalPPDReportRejectsUnknownFlag(t *testing.T) {
	resetGlobalFlags(t, []string{"chronos-code", "eval", "ppd", "--report", "--basline", "missing.json"})
	err := runEval()
	if err == nil || !strings.Contains(err.Error(), "invalid PPD report flag") {
		t.Fatalf("runEval() error = %v, want invalid report flag", err)
	}
}
