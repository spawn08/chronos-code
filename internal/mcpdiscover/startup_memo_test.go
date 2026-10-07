package mcpdiscover

import (
	"context"
	"errors"
	"testing"

	"github.com/spawn08/chronos/engine/mcp"
)

func TestStartupFailureMemoSkipsRepeatedConnects(t *testing.T) {
	failure := errors.New("server exited")
	created := 0
	memo := NewStartupFailureMemo(func(mcp.ServerConfig) (RuntimeClient, error) {
		created++
		return &poolTestClient{connect: func(context.Context) error { return failure }}, nil
	})
	first, err := memo.NewClient(poolConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Connect(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("first connect error = %v", err)
	}
	for i := 0; i < 9; i++ {
		if _, err := memo.NewClient(poolConfig()); !errors.Is(err, failure) {
			t.Fatalf("memoized client error = %v", err)
		}
	}
	other := poolConfig()
	other.Name = "other"
	if _, err := memo.NewClient(other); err != nil {
		t.Fatalf("a different server must not share the failure: %v", err)
	}
	if created != 2 {
		t.Fatalf("factory calls = %d, want 2", created)
	}

	memo.Stop()
	if _, err := memo.NewClient(poolConfig()); err != nil {
		t.Fatalf("after Stop a client must connect normally: %v", err)
	}
	if created != 3 {
		t.Fatalf("factory calls after Stop = %d, want 3", created)
	}
}
