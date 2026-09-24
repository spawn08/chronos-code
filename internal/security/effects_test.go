package security

import (
	"context"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
)

func TestGuardEnforcesExplicitEffectGrant(t *testing.T) {
	guard := NewGuard(defaultTestPolicy(), t.TempDir(), nil)
	readOnly := WithEffectGrant(context.Background(), EffectRead)
	for _, name := range []string{"file_read", "workspace_info", "codebase_search"} {
		if err := guard.Before(readOnly, &hooks.Event{Type: hooks.EventToolCallBefore, Name: name, Input: map[string]any{"path": "."}}); err != nil {
			t.Fatalf("read-only grant blocked %s: %v", name, err)
		}
	}
	for _, name := range []string{"file_write", "shell", "mcp__github__search", "unknown_dynamic_tool"} {
		err := guard.Before(readOnly, &hooks.Event{Type: hooks.EventToolCallBefore, Name: name, Input: map[string]any{"path": "out.txt", "command": "true"}})
		if err == nil || !strings.Contains(err.Error(), "undelegated effect") {
			t.Fatalf("read-only grant did not block %s: %v", name, err)
		}
	}
}

func TestEffectGrantIsCopiedFromContext(t *testing.T) {
	ctx := WithEffectGrant(context.Background(), EffectRead)
	grant, ok := EffectGrantFromContext(ctx)
	if !ok {
		t.Fatal("effect grant missing")
	}
	grant[EffectDeliveryWrite] = struct{}{}
	again, _ := EffectGrantFromContext(ctx)
	if _, ok := again[EffectDeliveryWrite]; ok {
		t.Fatal("caller mutated context effect grant")
	}
}
