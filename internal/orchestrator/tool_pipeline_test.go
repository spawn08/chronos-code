package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/incctx"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/toolcompress"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
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
	if def.ParallelSafe {
		t.Fatal("side-effecting user hooks must keep tool calls serial")
	}
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

type parallelReadProvider struct {
	guardTestProvider
	requests  []*model.ChatRequest
	toolCalls []model.ToolCall
}

func (p *parallelReadProvider) Name() string { return "fixture" }

func (p *parallelReadProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.requests = append(p.requests, req)
	if len(p.requests) == 1 {
		if len(p.toolCalls) > 0 {
			return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: p.toolCalls}, nil
		}
		return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{
			{ID: "read", Name: "file_read", Arguments: `{"path":"source.txt","start_line":1,"end_line":1}`},
			{ID: "grep", Name: "file_grep", Arguments: `{"path":"source.txt","pattern":"needle"}`},
		}}, nil
	}
	return &model.ChatResponse{Content: "done", StopReason: model.StopReasonEnd}, nil
}

func TestPipelineMixedReadWriteBatchKeepsDependencies(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := &parallelReadProvider{guardTestProvider: guardTestProvider{id: "gpt-4o"}, toolCalls: []model.ToolCall{
		{ID: "before", Name: "file_read", Arguments: `{"path":"source.txt"}`},
		{ID: "write", Name: "file_write", Arguments: `{"path":"source.txt","content":"after"}`},
		{ID: "after", Name: "file_read", Arguments: `{"path":"source.txt"}`},
	}}
	a := newExecutionTestAgent("pipeline", provider)
	a.Storage = pipelineAgent(t).Storage
	a.Tools.Register(builtins.NewFileReadTool(root))
	write := builtins.NewFileWriteTool(root)
	write.Permission = tool.PermAllow
	a.Tools.Register(write)
	incctx.Wrap(a, root)
	wrapToolPipeline(a, nil, config.HooksConfig{}, nil, nil)
	if _, err := a.Chat(context.Background(), "edit"); err != nil {
		t.Fatal(err)
	}
	messages := provider.requests[1].Messages
	results := messages[len(messages)-3:]
	for i, want := range map[int]string{0: "before", 2: "after"} {
		var result map[string]any
		if err := json.Unmarshal([]byte(results[i].Content), &result); err != nil || result["content"] != want {
			t.Fatalf("read %d: %s, want %q (err=%v)", i, results[i].Content, want, err)
		}
	}
}

func TestPipelineReadBatchRunsConcurrentlyInSDK(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("needle"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := &parallelReadProvider{guardTestProvider: guardTestProvider{id: "gpt-4o"}}
	a := newExecutionTestAgent("pipeline", provider)
	a.Storage = pipelineAgent(t).Storage
	a.Tools.Register(builtins.NewFileReadTool(root))
	a.Tools.Register(builtins.NewFileGrepTool(root))
	incctx.Wrap(a, root)
	incctx.WrapGrep(a, root)
	wrapToolPipeline(a, nil, config.HooksConfig{}, nil, nil)
	started := make(chan string, 2)
	release := make(chan struct{})
	for _, name := range []string{"file_read", "file_grep"} {
		def, _ := a.Tools.Get(name)
		orig := def.Handler
		def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			started <- name
			select {
			case <-release:
				return orig(ctx, args)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.Chat(ctx, "inspect"); done <- err }()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			<-done
			t.Fatal("both reads must start before either finishes")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("model calls = %d", len(provider.requests))
	}
	messages := provider.requests[1].Messages
	results := messages[len(messages)-2:]
	if results[0].ToolCallID != "read" || results[1].ToolCallID != "grep" || !strings.Contains(results[0].Content, "needle") || !strings.Contains(results[1].Content, "needle") {
		t.Fatalf("tool results lost content or model order: %+v", results)
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
