package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/incctx"
	"github.com/spawn08/chronos/engine/tool/builtins"
)

func TestFreshEditsRejectStaleSourceAndValidateWholeBatch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := pipelineAgent(t)
	a.Tools.Register(builtins.NewFileReadTool(root))
	a.Tools.Register(builtins.NewFileWriteTool(root))
	incctx.Wrap(a, root)
	o := &Orchestrator{cfg: &config.Config{Tools: config.ToolsConfig{FreshEdits: true}}}
	o.installFreshEdits(a, root)
	read, _ := a.Tools.Get("file_read")
	write, _ := a.Tools.Get("file_write")
	edit, _ := a.Tools.Get("file_edit")
	out, err := read.Handler(context.Background(), map[string]any{"path": "source.txt"})
	if err != nil {
		t.Fatal(err)
	}
	hash := out.(map[string]any)["content_hash"]
	if err := os.WriteFile(path, []byte("alpha\nbeta changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Handler(context.Background(), map[string]any{"path": "source.txt", "expected_hash": hash, "old_content": "alpha", "new_content": "new"}); err == nil {
		t.Fatal("stale edit accepted")
	}
	out, err = read.Handler(context.Background(), map[string]any{"path": "source.txt"})
	if err != nil {
		t.Fatal(err)
	}
	hash = out.(map[string]any)["content_hash"]
	if _, err := edit.Handler(context.Background(), map[string]any{"path": "source.txt", "expected_hash": hash, "edits": []any{map[string]any{"old_content": "alpha", "new_content": "new"}, map[string]any{"old_content": "not present", "new_content": "other"}}}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "alpha\nbeta changed\n" {
		t.Fatalf("partial batch wrote source: %q %v", data, err)
	}
}
