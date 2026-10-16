package incctx

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
)

func TestReadOffsetLimitAndTailReturnRequestedSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("first\nsecond\nlast"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &agent.Agent{Tools: tool.NewRegistry()}
	a.Tools.Register(builtins.NewFileReadTool(root))
	Wrap(a, root)
	read, _ := a.Tools.Get("file_read")
	for _, test := range []struct {
		offset   int
		expected string
	}{{2, "second"}, {-1, "last"}} {
		result, err := read.Handler(context.Background(), map[string]any{"path": "file.txt", "offset": test.offset, "limit": 1})
		if err != nil || result.(map[string]any)["content"] != test.expected {
			t.Fatalf("offset %d: %v %v", test.offset, result, err)
		}
	}
}
