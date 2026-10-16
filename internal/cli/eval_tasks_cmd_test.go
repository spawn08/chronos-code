package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestEvalTasksRejectsInvalidExecutionOptions(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--run", "--validate-only"}, {"--run", "--repetitions", "0"}, {"--run", "--repetitions", "101"},
		{"--run", "--output"}, {"--run", "--fixture-provider"}, {"--validate-only", "--output", "unused.jsonl"},
	} {
		if err := runEvalTasksTo(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted args %v", args)
		}
	}
}

func TestEvalTasksValidatesRuntimeManifest(t *testing.T) {
	var output bytes.Buffer
	if err := runEvalTasksTo([]string{"--validate-only", "--manifest", "../../benchmark/runtime/manifest.yaml"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "validated 1 tasks") {
		t.Fatalf("validation output: %s", output.String())
	}
}
