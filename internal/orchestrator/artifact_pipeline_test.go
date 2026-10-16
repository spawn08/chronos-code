package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/storage"
)

func TestArtifactByteEnvelopePreservesContinuation(t *testing.T) {
	content := strings.Repeat("\x00", 32768)
	result := artifactByteResult("shell:sh-fixture", content, 123, false)
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxToolResultBytes {
		t.Fatalf("chunk exceeds envelope cap: bytes=%d error=%v", len(encoded), err)
	}
	if result["next_byte_offset"].(int) != 123+len(result["content"].(string)) || result["truncated"] != true {
		t.Fatalf("continuation metadata lost: %v", result)
	}
}

func TestCompressedPipelineRetainsRecoverableArtifact(t *testing.T) {
	a := pipelineAgent(t)
	o := &Orchestrator{}
	o.installArtifacts(a)
	a.Tools.Register(&tool.Definition{Name: "report", Handler: func(context.Context, map[string]any) (any, error) {
		return map[string]any{"finding": strings.Repeat("earlier context\n", 10000) + "CRITICAL_LATE_FINDING"}, nil
	}})
	wrapToolPipeline(a, nil, config.HooksConfig{}, nil, nil)
	ctx := storage.WithSession(context.Background(), "artifact-pipeline")
	report, _ := a.Tools.Get("report")
	out, err := report.Handler(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	fields := out.(map[string]any)
	id, ok := fields["artifact_id"].(string)
	if fields["compressed"] != true || !ok || id == "" {
		t.Fatalf("missing compressed artifact reference: %v", fields)
	}
	read, _ := a.Tools.Get("read_artifact")
	recovered, err := read.Handler(ctx, map[string]any{"artifact_id": id, "byte_offset": -100, "max_bytes": 100})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recovered.(map[string]any)["content"].(string), "CRITICAL_LATE_FINDING") {
		t.Fatalf("late finding lost: %v", recovered)
	}
	if _, err := read.Handler(storage.WithSession(ctx, "other"), map[string]any{"artifact_id": id}); err == nil {
		t.Fatal("artifact crossed session boundary")
	}
}
