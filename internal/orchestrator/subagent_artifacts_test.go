package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spawn08/chronos/sdk/harness"
	"github.com/spawn08/chronos/storage"
)

func TestOversizedSubagentFullReportRemainsRecoverable(t *testing.T) {
	a := pipelineAgent(t)
	original := strings.Repeat("earlier finding\n", subagentResultLimit/8) + "CRITICAL_LATE_FINDING"
	runner := &configuredAgentRunner{store: a.Storage, fallback: resourceRunnerFunc(func(context.Context, harness.SubAgentSpec, string) (string, error) { return original, nil })}
	ctx := storage.WithSession(context.Background(), "subagent-artifact")
	_, err := runner.Run(ctx, harness.SubAgentSpec{Name: "dynamic"}, "inspect")
	var sizeErr *subagentSizeError
	if !errors.As(err, &sizeErr) || sizeErr.Artifact == "" {
		t.Fatalf("oversized result lost recovery reference: %v", err)
	}
	o := &Orchestrator{}
	o.installArtifacts(a)
	read, _ := a.Tools.Get("read_artifact")
	recovered, err := read.Handler(ctx, map[string]any{"artifact_id": sizeErr.Artifact, "byte_offset": -200, "max_bytes": 200})
	if err != nil || !strings.Contains(errorFreeJSON(recovered), "CRITICAL_LATE_FINDING") {
		t.Fatalf("late report lost: %v error=%v", recovered, err)
	}
}

func TestInterruptedSubagentErrorIncludesUsefulEvidence(t *testing.T) {
	a := pipelineAgent(t)
	cause := errors.New("provider disconnected")
	runner := &configuredAgentRunner{store: a.Storage, fallback: resourceRunnerFunc(func(context.Context, harness.SubAgentSpec, string) (string, error) {
		return "Found the regression in source.go:19; next check the caller.", cause
	})}
	_, err := runner.Run(storage.WithSession(context.Background(), "partial-artifact"), harness.SubAgentSpec{Name: "dynamic"}, "inspect")
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "source.go:19") || !strings.Contains(err.Error(), "artifact_id=subagent:") {
		t.Fatalf("SDK-visible error lost partial evidence: %v", err)
	}
}
