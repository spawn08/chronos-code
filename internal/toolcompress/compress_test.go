package toolcompress

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage/adapters/sqlite"
)

type fakeProvider struct{}

func (fakeProvider) Chat(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
	return nil, nil
}
func (fakeProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, nil
}
func (fakeProvider) Name() string  { return "fake" }
func (fakeProvider) Model() string { return "claude-sonnet-4-6" }

func newTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlite.New(dsn)
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	a := &agent.Agent{
		ID:      "test-agent",
		Model:   fakeProvider{},
		Tools:   tool.NewRegistry(),
		Storage: store,
	}
	return a
}

func TestWrapCompressesLargeResult(t *testing.T) {
	a := newTestAgent(t)

	// Word-like content with natural token boundaries, well over both the
	// compression threshold and maxStoredResultChunkBytes. A single huge
	// run of one repeated character instead would be a BPE tokenizer worst
	// case (unbounded merge-pair growth with no boundaries to stop at) and
	// makes CountString pathologically slow at this size.
	big := strings.Repeat("hello world ", 25_000)
	a.Tools.Register(&tool.Definition{
		Name:       "big_tool",
		Permission: tool.PermAllow,
		Handler: func(context.Context, map[string]any) (any, error) {
			return map[string]any{"data": big}, nil
		},
	})
	a.Tools.Register(&tool.Definition{
		Name:       "small_tool",
		Permission: tool.PermAllow,
		Handler: func(context.Context, map[string]any) (any, error) {
			return map[string]any{"data": "short"}, nil
		},
	})

	Wrap(a, 0)

	ctx := context.Background()

	out, err := a.Tools.Execute(ctx, "big_tool", nil)
	if err != nil {
		t.Fatalf("execute big_tool: %v", err)
	}
	res, ok := out.(map[string]any)
	if !ok || res["compressed"] != true {
		t.Fatalf("expected big_tool result to be compressed, got %#v", out)
	}
	key, _ := res["storage_key"].(string)
	if key == "" {
		t.Fatal("expected a non-empty storage_key")
	}

	out, err = a.Tools.Execute(ctx, "small_tool", nil)
	if err != nil {
		t.Fatalf("execute small_tool: %v", err)
	}
	res, ok = out.(map[string]any)
	if !ok || res["data"] != "short" {
		t.Fatalf("expected small_tool result to pass through uncompressed, got %#v", out)
	}

	chunk, err := a.Tools.Execute(ctx, ReadStoredResultTool, map[string]any{"key": key})
	if err != nil {
		t.Fatalf("execute %s: %v", ReadStoredResultTool, err)
	}
	chunkMap, ok := chunk.(map[string]any)
	if !ok {
		t.Fatalf("read_stored_result result = %#v, want chunk metadata", chunk)
	}
	content, _ := chunkMap["content"].(string)
	if len(content) != defaultStoredResultChunkBytes || chunkMap["truncated"] != true {
		t.Fatalf("default chunk = %d bytes, truncated=%v", len(content), chunkMap["truncated"])
	}
	next, _ := chunkMap["next_offset"].(int)
	chunk, err = a.Tools.Execute(ctx, ReadStoredResultTool, map[string]any{
		"key": key, "offset": next, "max_bytes": 100_000,
	})
	if err != nil {
		t.Fatalf("execute second %s chunk: %v", ReadStoredResultTool, err)
	}
	chunkMap = chunk.(map[string]any)
	if got := len(chunkMap["content"].(string)); got > maxStoredResultChunkBytes {
		t.Fatalf("requested oversized chunk returned %d bytes, cap is %d", got, maxStoredResultChunkBytes)
	}

	var reconstructed strings.Builder
	offset := 0
	for {
		chunk, err = a.Tools.Execute(ctx, ReadStoredResultTool, map[string]any{
			"key": key, "offset": offset, "max_bytes": 4096,
		})
		if err != nil {
			t.Fatalf("paginate %s: %v", ReadStoredResultTool, err)
		}
		page := chunk.(map[string]any)
		reconstructed.WriteString(page["content"].(string))
		offset = page["next_offset"].(int)
		if page["truncated"] != true {
			break
		}
	}
	if !strings.Contains(reconstructed.String(), big) {
		t.Fatal("paginated read_stored_result did not preserve the full original data")
	}
}
