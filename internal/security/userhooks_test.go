package security

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
)

func TestUserHookExpansionIsInjectionSafe(t *testing.T) {
	for _, command := range []string{
		"printf '%s' {{user_message}}",
		"printf '%s' '{{user_message}}'",
		`printf '%s' "{{user_message}}"`,
	} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, "injected")
			value := "$(touch " + marker + "); hello'; printf bad"

			result, err := ExecuteHook(context.Background(), root, config.HookDef{
				Name:      "safe",
				Command:   command,
				TimeoutMs: 1000,
			}, map[string]any{"user_message": value})
			if err != nil {
				t.Fatalf("ExecuteHook: %v", err)
			}
			if got := strings.Join(result.Stdout.Lines, "\n"); got != value {
				t.Fatalf("stdout = %q, want %q", got, value)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("injected command created marker: %v", err)
			}
		})
	}
}

func TestUserHookStructuredPlaceholdersAreJSON(t *testing.T) {
	result, err := ExecuteHook(context.Background(), t.TempDir(), config.HookDef{
		Name:      "json",
		Command:   "printf '%s\\n%s' {{tool_args}} {{tool_output}}",
		TimeoutMs: 1000,
	}, map[string]any{
		"tool_args":   map[string]any{"path": "a'b", "force": true},
		"tool_output": []any{"ok", 2},
	})
	if err != nil {
		t.Fatalf("ExecuteHook: %v", err)
	}
	want := []string{`{"force":true,"path":"a'b"}`, `["ok",2]`}
	if !reflect.DeepEqual(result.Stdout.Lines, want) {
		t.Fatalf("stdout = %#v, want %#v", result.Stdout.Lines, want)
	}
}

func TestUserHookUnknownPlaceholderFailsClearly(t *testing.T) {
	result, err := ExecuteHook(context.Background(), t.TempDir(), config.HookDef{
		Name:      "unknown",
		Command:   "printf '%s' {{not_supported}}",
		TimeoutMs: 1000,
	}, nil)
	if !errors.Is(err, ErrHookTemplate) || !strings.Contains(err.Error(), `unknown placeholder "not_supported"`) {
		t.Fatalf("error = %v, want unknown template placeholder", err)
	}
	if result.Status != HookTemplateError {
		t.Fatalf("status = %q, want %q", result.Status, HookTemplateError)
	}
}

func TestHookRunnerUsesCanonicalWorkspace(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	result, err := ExecuteHook(context.Background(), link, config.HookDef{
		Name: "pwd", Command: "pwd", TimeoutMs: 1000,
	}, nil)
	if err != nil {
		t.Fatalf("ExecuteHook: %v", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	if got := strings.Join(result.Stdout.Lines, "\n"); got != canonicalRoot {
		t.Fatalf("pwd = %q, want canonical root %q", got, canonicalRoot)
	}
}

func TestHookRunnerDistinguishesFailures(t *testing.T) {
	t.Run("nonzero", func(t *testing.T) {
		result, err := ExecuteHook(context.Background(), t.TempDir(), config.HookDef{
			Name: "fail", Command: "printf denied >&2; exit 7", TimeoutMs: 1000,
		}, nil)
		if !errors.Is(err, ErrHookExit) || result.Status != HookExitedNonZero || result.ExitCode != 7 {
			t.Fatalf("result = %#v, error = %v", result, err)
		}
		if got := strings.Join(result.Stderr.Lines, "\n"); got != "denied" {
			t.Fatalf("stderr = %q, want denied", got)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		started := time.Now()
		result, err := ExecuteHook(context.Background(), t.TempDir(), config.HookDef{
			Name: "slow", Command: "sleep 5", TimeoutMs: 30,
		}, nil)
		if !errors.Is(err, ErrHookTimeout) || result.Status != HookTimedOut {
			t.Fatalf("result = %#v, error = %v", result, err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("timeout took %v", elapsed)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := ExecuteHook(ctx, t.TempDir(), config.HookDef{
			Name: "canceled", Command: "sleep 5", TimeoutMs: 1000,
		}, nil)
		if !errors.Is(err, ErrHookCanceled) || result.Status != HookCanceled {
			t.Fatalf("result = %#v, error = %v", result, err)
		}
	})

	t.Run("spawn", func(t *testing.T) {
		root := t.TempDir()
		runner, err := NewHookRunner(root)
		if err != nil {
			t.Fatalf("NewHookRunner: %v", err)
		}
		if err := os.Remove(root); err != nil {
			t.Fatalf("remove workspace: %v", err)
		}
		result, err := runner.Run(context.Background(), config.HookDef{
			Name: "spawn", Command: ":", TimeoutMs: 1000,
		}, nil)
		if !errors.Is(err, ErrHookSpawn) || result.Status != HookSpawnError {
			t.Fatalf("result = %#v, error = %v", result, err)
		}
	})
}

func TestHookRunnerBoundsStdoutAndStderr(t *testing.T) {
	command := "i=0; while [ $i -lt 510 ]; do printf 'out-%s\\n' \"$i\"; printf 'err-%s\\n' \"$i\" >&2; i=$((i+1)); done"
	result, err := ExecuteHook(context.Background(), t.TempDir(), config.HookDef{
		Name: "bounded", Command: command, TimeoutMs: 2000,
	}, nil)
	if err != nil {
		t.Fatalf("ExecuteHook: %v", err)
	}
	for name, output := range map[string]CapturedOutput{"stdout": result.Stdout, "stderr": result.Stderr} {
		if len(output.Lines) != CaptureLineLimit || output.TotalLines != 510 || output.OmittedLines != 10 || !output.Truncated {
			t.Errorf("%s = %#v", name, output)
		}
	}
	if result.Stdout.Lines[0] != "out-10" || result.Stderr.Lines[0] != "err-10" {
		t.Fatalf("bounded tails start at stdout %q, stderr %q", result.Stdout.Lines[0], result.Stderr.Lines[0])
	}
}
