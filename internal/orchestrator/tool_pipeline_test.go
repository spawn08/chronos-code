package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/incctx"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/toolcompress"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage/adapters/sqlite"
)

func pipelineAgent(t *testing.T) *agent.Agent {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &agent.Agent{ID: "pipeline", Model: &guardTestProvider{id: "gpt-4o"}, Tools: tool.NewRegistry(), Storage: s}
}

func TestPipelineReadsRangeBeforeCompressionAndRunsHooks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(strings.Repeat("hello world\n", 20000)), 0600); err != nil {
		t.Fatal(err)
	}
	a := pipelineAgent(t)
	a.Tools.Register(&tool.Definition{Name: "file_read", Handler: func(context.Context, map[string]any) (any, error) {
		t.Fatal("whole-file precursor must not run")
		return nil, nil
	}})
	incctx.Wrap(a, root)
	runner, err := security.NewHookRunner(root)
	if err != nil {
		t.Fatal(err)
	}
	hooks := config.HooksConfig{
		PreToolCall:  []config.HookDef{{Name: "pre", Command: "printf p >> hooks.log", TimeoutMs: 1000}},
		PostToolCall: []config.HookDef{{Name: "post", Command: "printf q >> hooks.log", TimeoutMs: 1000}},
	}
	wrapToolPipeline(a, nil, hooks, runner, &hookActivityTracker{})
	def, _ := a.Tools.Get("file_read")
	out, err := def.Handler(context.Background(), map[string]any{"path": "large.txt", "start_line": 10, "end_line": 11})
	if err != nil {
		t.Fatal(err)
	}
	result := out.(map[string]any)
	if result["compressed"] == true || strings.TrimSuffix(result["content"].(string), "\n") != "hello world\nhello world" {
		t.Fatalf("unexpected range: %#v", result)
	}
	data, err := os.ReadFile(filepath.Join(root, "hooks.log"))
	if err != nil || string(data) != "pq" {
		t.Fatalf("hooks executed incorrectly: %q, %v", data, err)
	}
}

func TestPipelineLateToolsCompressedAndReaderOffsetsPreserved(t *testing.T) {
	a := pipelineAgent(t)
	wrapToolPipeline(a, nil, config.HooksConfig{}, nil, nil)
	before := registeredToolNameSet(a)
	calls := 0
	a.Tools.Register(&tool.Definition{Name: "mcp_large", Handler: func(context.Context, map[string]any) (any, error) {
		calls++
		return map[string]any{"text": strings.Repeat("\"hello\"\n", 50000)}, nil
	}})
	wrapLateTools(a, before, &Orchestrator{})
	def, _ := a.Tools.Get("mcp_large")
	out, err := def.Handler(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	result := out.(map[string]any)
	if result["compressed"] != true || calls != 1 {
		t.Fatalf("late pipeline result=%#v calls=%d", result, calls)
	}
	reader, _ := a.Tools.Get(toolcompress.ReadStoredResultTool)
	chunk, err := reader.Handler(context.Background(), map[string]any{"key": result["storage_key"], "max_bytes": 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	m := chunk.(map[string]any)
	encoded, _ := json.Marshal(m)
	if len(encoded) > maxToolResultBytes || m["content"] == nil || m["next_offset"].(int) <= 0 || m["storage_key"] != result["storage_key"] {
		t.Fatalf("invalid chunk envelope: %d bytes, keys=%v", len(encoded), m["next_offset"])
	}
	before = registeredToolNameSet(a)
	wrapLateTools(a, before, &Orchestrator{})
	def, _ = a.Tools.Get("mcp_large")
	if _, err := def.Handler(context.Background(), nil); err != nil || calls != 2 {
		t.Fatalf("reinstallation: calls=%d err=%v", calls, err)
	}
}
