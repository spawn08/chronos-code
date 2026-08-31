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

	big := strings.Repeat("x", 5000) // well over the 500-token default threshold
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

	full, err := a.Tools.Execute(ctx, ReadStoredResultTool, map[string]any{"key": key})
	if err != nil {
		t.Fatalf("execute %s: %v", ReadStoredResultTool, err)
	}
	fullStr, _ := full.(string)
	if !strings.Contains(fullStr, big) {
		t.Fatalf("read_stored_result did not return the full original data")
	}
}
