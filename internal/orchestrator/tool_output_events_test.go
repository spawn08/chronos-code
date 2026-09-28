package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos/engine/model"
	chronosstream "github.com/spawn08/chronos/engine/stream"
	"github.com/spawn08/chronos/engine/tool"
)

// The TUI must see what the tool produced, not the compressed stub the
// model receives.
func TestToolOutputEventCarriesResultBeforeCompression(t *testing.T) {
	provider := &parallelReadProvider{guardTestProvider: guardTestProvider{id: "gpt-4o"}, toolCalls: []model.ToolCall{
		{ID: "call-1", Name: "shell", Arguments: `{"command":"go test ./..."}`},
	}}
	a := newExecutionTestAgent("pipeline", provider)
	a.Storage = pipelineAgent(t).Storage
	a.Broker = chronosstream.NewBroker(chronosstream.WithBufferSize(16))
	stdout := strings.Repeat("ok  package/line\n", 5000)
	a.Tools.Register(&tool.Definition{Name: "shell", Permission: tool.PermAllow, Handler: func(context.Context, map[string]any) (any, error) {
		return map[string]any{"stdout": stdout, "stderr": "", "exit_code": 0}, nil
	}})
	wrapToolPipeline(a, nil, config.HooksConfig{}, nil, nil)
	events := a.Broker.Subscribe("test")
	if _, err := a.Chat(context.Background(), "run tests"); err != nil {
		t.Fatal(err)
	}
	var output, result map[string]any
	for output == nil || result == nil {
		evt := <-events
		data, _ := evt.Data.(map[string]any)
		switch evt.Type {
		case EventToolOutput:
			if result != nil {
				t.Fatal("tool_output must precede tool_result")
			}
			if data["id"] != "call-1" || data["tool"] != "shell" || data["agent"] != "pipeline" {
				t.Fatalf("tool_output identity = %#v", data)
			}
			output, _ = data["output"].(map[string]any)
		case chronosstream.EventToolResult:
			result, _ = data["result"].(map[string]any)
		}
	}
	if output["stdout"] != stdout {
		t.Fatalf("tool_output lost the real stdout (%d bytes)", len(output["stdout"].(string)))
	}
	if result["compressed"] != true {
		t.Fatalf("fixture must exercise compression, got %#v", result)
	}
}

func TestBoundToolOutputCapsStringsWithoutMutating(t *testing.T) {
	large := strings.Repeat("x", maxToolOutputEventString+10)
	original := map[string]any{"stdout": large, "exit_code": 1}
	bounded := boundToolOutput(original).(map[string]any)
	if original["stdout"] != large {
		t.Fatal("bounding mutated the model-facing result")
	}
	if got := bounded["stdout"].(string); len(got) >= len(large)+50 || !strings.HasSuffix(got, "[output truncated for display]") {
		t.Fatalf("stdout not capped: %d bytes", len(got))
	}
	if bounded["exit_code"] != 1 {
		t.Fatal("non-string fields must pass through")
	}
}
