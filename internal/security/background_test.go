package security

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos/storage"
)

func TestBackgroundShellLifecycle(t *testing.T) {
	ctx := storage.WithSession(context.Background(), "s1")
	shells := NewBackgroundShells(t.TempDir())
	defer shells.Close()
	sh := NewBackgroundShellTool(t.TempDir(), time.Minute, nil, shells)
	await := NewAwaitShellTool(shells)
	kill := NewKillShellTool(shells)

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"fast command returns its result", func(t *testing.T) {
			out, err := sh.Handler(ctx, map[string]any{"command": "echo hi; exit 3"})
			if err != nil {
				t.Fatal(err)
			}
			r := out.(map[string]any)
			if r["status"] != "exited" || r["exit_code"] != 3 || strings.TrimSpace(r["output"].(string)) != "hi" {
				t.Fatalf("result = %v", r)
			}
		}},
		{"slow command is backgrounded, not killed, then awaited by pattern and exit", func(t *testing.T) {
			out, err := sh.Handler(ctx, map[string]any{"command": "echo start; sleep 1; echo READY; sleep 1; echo done", "block_until_ms": 100.0})
			if err != nil {
				t.Fatal(err)
			}
			r := out.(map[string]any)
			if r["status"] != "running" {
				t.Fatalf("status = %v, want running", r["status"])
			}
			id := r["shell_id"].(string)
			got, err := await.Handler(ctx, map[string]any{"shell_id": id, "pattern": "READY", "timeout_ms": 5000.0})
			if err != nil {
				t.Fatal(err)
			}
			g := got.(map[string]any)
			if g["matched"] != true || !strings.Contains(g["output"].(string), "READY") || strings.Contains(g["output"].(string), "start") {
				t.Fatalf("pattern await = %v (want only new output, containing READY)", g)
			}
			got, err = await.Handler(ctx, map[string]any{"shell_id": id, "timeout_ms": 5000.0})
			if err != nil {
				t.Fatal(err)
			}
			g = got.(map[string]any)
			if g["status"] != "exited" || g["exit_code"] != 0 || !strings.Contains(g["output"].(string), "done") {
				t.Fatalf("exit await = %v", g)
			}
			log, err := os.ReadFile(g["log_path"].(string))
			if err != nil || !strings.Contains(string(log), "# exit_code: 0") || !strings.Contains(string(log), "READY") {
				t.Fatalf("log = %q, %v", log, err)
			}
			if _, err := await.Handler(ctx, map[string]any{"shell_id": id}); err == nil {
				t.Fatal("a reported, exited shell should be forgotten")
			}
		}},
		{"kill stops a hung command", func(t *testing.T) {
			out, _ := sh.Handler(ctx, map[string]any{"command": "sleep 60", "block_until_ms": 0.0})
			id := out.(map[string]any)["shell_id"].(string)
			if lines := shells.Running("s1"); len(lines) != 1 || !strings.Contains(lines[0], id) {
				t.Fatalf("Running = %v", lines)
			}
			start := time.Now()
			got, err := kill.Handler(ctx, map[string]any{"shell_id": id})
			if err != nil {
				t.Fatal(err)
			}
			if g := got.(map[string]any); g["killed"] != true || g["status"] != "exited" || time.Since(start) > 3*time.Second {
				t.Fatalf("kill = %v after %v", g, time.Since(start))
			}
		}},
		{"other sessions cannot see a shell", func(t *testing.T) {
			out, _ := sh.Handler(ctx, map[string]any{"command": "sleep 5", "block_until_ms": 0.0})
			id := out.(map[string]any)["shell_id"].(string)
			other := storage.WithSession(context.Background(), "s2")
			if _, err := await.Handler(other, map[string]any{"shell_id": id, "timeout_ms": 1000.0}); err == nil {
				t.Fatal("another session awaited this shell")
			}
			_, _ = kill.Handler(ctx, map[string]any{"shell_id": id})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestTailOutputKeepsEnd(t *testing.T) {
	s := strings.Repeat("a", shellOutputTail) + "END"
	got, truncated := tailOutput(s)
	if !truncated || !strings.HasSuffix(got, "END") || len(got) > shellOutputTail+64 {
		t.Fatalf("tailOutput: truncated=%v len=%d", truncated, len(got))
	}
}

// Concurrent awaits on one shell (await_shell is parallel-safe) must not
// race on the read offset, and each output byte is returned once.
func TestConcurrentAwaitsReturnOutputOnce(t *testing.T) {
	ctx := storage.WithSession(context.Background(), "s1")
	shells := NewBackgroundShells(t.TempDir())
	defer shells.Close()
	sh := NewBackgroundShellTool(t.TempDir(), time.Minute, nil, shells)
	await := NewAwaitShellTool(shells)
	out, err := sh.Handler(ctx, map[string]any{"command": "for i in 1 2 3 4 5; do echo line$i; sleep 0.1; done", "block_until_ms": 0.0})
	if err != nil {
		t.Fatal(err)
	}
	id := out.(map[string]any)["shell_id"].(string)
	results := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r, err := await.Handler(ctx, map[string]any{"shell_id": id, "pattern": "line", "timeout_ms": 3000.0})
			if err != nil {
				results <- ""
				return
			}
			results <- r.(map[string]any)["output"].(string)
		}()
	}
	combined := <-results + <-results
	rest, _ := await.Handler(ctx, map[string]any{"shell_id": id, "timeout_ms": 3000.0})
	if rest != nil {
		combined += rest.(map[string]any)["output"].(string)
	}
	for i := 1; i <= 5; i++ {
		if n := strings.Count(combined, fmt.Sprintf("line%d\n", i)); n != 1 {
			t.Errorf("line%d returned %d times in %q", i, n, combined)
		}
	}
}
