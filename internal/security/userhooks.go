package security

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
)

const hookWaitDelay = 100 * time.Millisecond

var (
	ErrHookTemplate = errors.New("hook template error")
	ErrHookSpawn    = errors.New("hook spawn error")
	ErrHookTimeout  = errors.New("hook timed out")
	ErrHookCanceled = errors.New("hook canceled")
	ErrHookExit     = errors.New("hook exited non-zero")

	hookPlaceholder = regexp.MustCompile(`\{\{([^{}]*)\}\}`)
)

// HookStatus describes the terminal state of a hook command.
type HookStatus string

const (
	HookSucceeded     HookStatus = "succeeded"
	HookTemplateError HookStatus = "template_error"
	HookSpawnError    HookStatus = "spawn_error"
	HookTimedOut      HookStatus = "timed_out"
	HookCanceled      HookStatus = "canceled"
	HookExitedNonZero HookStatus = "exited_non_zero"
)

// HookResult contains bounded output and deterministic command status.
type HookResult struct {
	Status   HookStatus
	ExitCode int
	Stdout   CapturedOutput
	Stderr   CapturedOutput
}

// HookError identifies why a hook did not complete successfully.
type HookError struct {
	Kind     error
	HookName string
	ExitCode int
	Cause    error
}

func (e *HookError) Error() string {
	switch e.Kind {
	case ErrHookExit:
		return fmt.Sprintf("hook %q exited with status %d", e.HookName, e.ExitCode)
	case ErrHookTimeout, ErrHookCanceled:
		return fmt.Sprintf("hook %q: %v", e.HookName, e.Kind)
	default:
		return fmt.Sprintf("hook %q: %v: %v", e.HookName, e.Kind, e.Cause)
	}
}

// Unwrap makes both the failure category and its underlying cause available
// through errors.Is/errors.As.
func (e *HookError) Unwrap() []error {
	if e.Cause == nil {
		return []error{e.Kind}
	}
	return []error{e.Kind, e.Cause}
}

// HookRunner executes hooks from one canonical workspace directory.
type HookRunner struct {
	root string
}

// NewHookRunner resolves workspaceRoot once and requires it to be a directory.
func NewHookRunner(workspaceRoot string) (*HookRunner, error) {
	absRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve hook workspace %q: %w", workspaceRoot, err)
	}
	root, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve hook workspace %q: %w", workspaceRoot, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat hook workspace %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("hook workspace %q is not a directory", root)
	}
	return &HookRunner{root: root}, nil
}

// ExpandTemplate substitutes supported placeholders as single shell words.
// tool_args and tool_output are JSON encoded before quoting.
func ExpandTemplate(command string, vars map[string]any) (string, error) {
	matches := hookPlaceholder.FindAllStringSubmatchIndex(command, -1)
	if len(matches) == 0 {
		return command, nil
	}

	var expanded strings.Builder
	state := shellState{}
	position := 0
	for _, match := range matches {
		prefix := command[position:match[0]]
		expanded.WriteString(prefix)
		state = parseShellState(prefix, state)

		name := strings.TrimSpace(command[match[2]:match[3]])
		if state.escaped {
			return "", fmt.Errorf("%w: placeholder %q cannot follow a shell escape", ErrHookTemplate, name)
		}
		value, err := hookPlaceholderValue(name, vars)
		if err != nil {
			return "", err
		}
		quoted := quoteShellWord(value)
		switch state.quote {
		case '\'':
			expanded.WriteByte('\'')
			expanded.WriteString(quoted)
			expanded.WriteByte('\'')
		case '"':
			expanded.WriteByte('"')
			expanded.WriteString(quoted)
			expanded.WriteByte('"')
		case '`':
			return "", fmt.Errorf("%w: placeholder %q is not supported inside backticks", ErrHookTemplate, name)
		default:
			expanded.WriteString(quoted)
		}
		position = match[1]
	}
	expanded.WriteString(command[position:])
	return expanded.String(), nil
}

func hookPlaceholderValue(name string, vars map[string]any) (string, error) {
	value, supplied := vars[name]
	switch name {
	case "tool_args", "tool_output":
		if !supplied {
			value = nil
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", fmt.Errorf("%w: encode placeholder %q as JSON: %v", ErrHookTemplate, name, err)
		}
		return string(encoded), nil
	case "tool_name", "session_id", "agent_id", "user_message":
		if !supplied || value == nil {
			return "", nil
		}
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("%w: placeholder %q requires a string value", ErrHookTemplate, name)
		}
		return text, nil
	default:
		return "", fmt.Errorf("%w: unknown placeholder %q", ErrHookTemplate, name)
	}
}

type shellState struct {
	quote   byte
	escaped bool
}

func parseShellState(command string, state shellState) shellState {
	for i := 0; i < len(command); i++ {
		char := command[i]
		if state.escaped {
			state.escaped = false
			continue
		}
		switch state.quote {
		case '\'':
			if char == '\'' {
				state.quote = 0
			}
		case '"':
			switch char {
			case '\\':
				state.escaped = true
			case '"':
				state.quote = 0
			}
		case '`':
			switch char {
			case '\\':
				state.escaped = true
			case '`':
				state.quote = 0
			}
		default:
			switch char {
			case '\\':
				state.escaped = true
			case '\'', '"', '`':
				state.quote = char
			}
		}
	}
	return state
}

func quoteShellWord(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// Run expands and executes def with its configured timeout. The shell is the
// host's POSIX shell; no runtime or interpreter is installed by the runner.
func (r *HookRunner) Run(ctx context.Context, def config.HookDef, vars map[string]any) (HookResult, error) {
	result := HookResult{ExitCode: -1}
	command, err := ExpandTemplate(def.Command, vars)
	if err != nil {
		result.Status = HookTemplateError
		return result, &HookError{Kind: ErrHookTemplate, HookName: def.Name, ExitCode: -1, Cause: err}
	}

	stdout := NewCaptureWriter()
	stderr := NewCaptureWriter()
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(def.TimeoutMs)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", command)
	cmd.Dir = r.root
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = hookWaitDelay
	err = cmd.Run()
	result.Stdout = stdout.Snapshot()
	result.Stderr = stderr.Snapshot()

	if err == nil {
		result.Status = HookSucceeded
		result.ExitCode = 0
		return result, nil
	}
	if ctx.Err() != nil {
		result.Status = HookCanceled
		return result, &HookError{Kind: ErrHookCanceled, HookName: def.Name, ExitCode: -1, Cause: ctx.Err()}
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		result.Status = HookTimedOut
		return result, &HookError{Kind: ErrHookTimeout, HookName: def.Name, ExitCode: -1, Cause: runCtx.Err()}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.Status = HookExitedNonZero
		result.ExitCode = exitErr.ExitCode()
		return result, &HookError{Kind: ErrHookExit, HookName: def.Name, ExitCode: result.ExitCode, Cause: err}
	}
	result.Status = HookSpawnError
	return result, &HookError{Kind: ErrHookSpawn, HookName: def.Name, ExitCode: -1, Cause: err}
}

// ExecuteHook is a convenience for one-off execution in workspaceRoot.
func ExecuteHook(ctx context.Context, workspaceRoot string, def config.HookDef, vars map[string]any) (HookResult, error) {
	runner, err := NewHookRunner(workspaceRoot)
	if err != nil {
		result := HookResult{Status: HookSpawnError, ExitCode: -1}
		return result, &HookError{Kind: ErrHookSpawn, HookName: def.Name, ExitCode: -1, Cause: err}
	}
	return runner.Run(ctx, def, vars)
}
