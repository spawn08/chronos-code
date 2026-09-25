package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/execution"
)

type fileState struct {
	hash   string
	size   int64
	exists bool
}

func wrapVerificationEvidence(a *agent.Agent) {
	if a == nil || a.Tools == nil {
		return
	}
	for _, definition := range a.Tools.List() {
		if definition == nil || definition.Handler == nil {
			continue
		}
		wrapped := *definition
		original := definition.Handler
		wrapped.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			runtime, enabled := taskRuntimeFromContext(ctx)
			if !enabled {
				return original(ctx, args)
			}
			if err := runtime.beforeToolCall(); err != nil {
				return nil, err
			}
			startedAt := time.Now().UTC()
			var before fileState
			var path string
			if wrapped.Name == "file_write" {
				path, _ = args["path"].(string)
				before = readFileState(runtime.workspaceRoot, path)
			}
			result, err := original(ctx, args)
			completedAt := time.Now().UTC()
			switch wrapped.Name {
			case "file_write":
				evidencePath := path
				if values, ok := result.(map[string]any); ok {
					if resolved, ok := values["path"].(string); ok && resolved != "" {
						evidencePath = resolved
					}
				}
				after := readFileState(runtime.workspaceRoot, evidencePath)
				if after.exists && (err == nil || after != before) {
					if _, recordErr := runtime.recordWrite(evidencePath, after.hash, after.size, execution.ProvenanceRuntime, completedAt); recordErr != nil {
						return result, errors.Join(err, fmt.Errorf("record file write evidence: %w", recordErr))
					}
					if refreshErr := runtime.refreshClaims(completedAt, evidencePath); refreshErr != nil {
						return result, errors.Join(err, refreshErr)
					}
				} else if err == nil && !after.exists {
					return result, fmt.Errorf("record file write evidence: written file %q is unavailable", path)
				}
			case "file_read":
				if err == nil {
					runtime.recordReadClaim(args, result)
				}
			case "shell", "shell_auto":
				if recordErr := recordShellEvidence(runtime, ctx, args, result, err, startedAt, completedAt); recordErr != nil {
					return result, errors.Join(err, recordErr)
				}
			}
			return result, err
		}
		a.Tools.Register(&wrapped)
	}
}

func readFileState(root, path string) fileState {
	if path == "" {
		return fileState{}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileState{}
	}
	sum := sha256.Sum256(data)
	return fileState{hash: hex.EncodeToString(sum[:]), size: int64(len(data)), exists: true}
}

func recordShellEvidence(runtime *taskRuntime, ctx context.Context, args map[string]any, result any, callErr error, startedAt, completedAt time.Time) error {
	command, _ := args["command"].(string)
	class := classifyShellCommand(command)
	terminal := execution.TerminalExited
	var exitCode *int
	if values, ok := result.(map[string]any); ok {
		if code, ok := values["exit_code"].(int); ok {
			exitCode = &code
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		terminal = execution.TerminalTimedOut
	case errors.Is(ctx.Err(), context.Canceled):
		terminal = execution.TerminalCancelled
	case callErr != nil:
		terminal = execution.TerminalSpawnFailed
	}
	if class == execution.CommandMutation || class == execution.CommandUnknown {
		if _, err := runtime.recordWorkspaceMutation(command, completedAt); err != nil {
			return err
		}
		// A mutating or unclassified command may have touched any file.
		return runtime.refreshClaims(completedAt)
	}
	_, err := runtime.recordCommand(command, class, exitCode, terminal, nil, execution.ProvenanceRuntime, startedAt, completedAt)
	return err
}

func classifyShellCommand(command string) execution.CommandClass {
	command = strings.TrimSpace(strings.ToLower(command))
	if command == "" {
		return execution.CommandUnknown
	}
	if strings.ContainsAny(command, ";|><") || strings.Contains(command, "&&") || strings.Contains(command, "||") {
		return execution.CommandMutation
	}
	for _, prefix := range []string{"go test", "pytest", "python -m pytest", "npm test", "npm run test", "bun test", "cargo test", "mvn test", "make test"} {
		if strings.HasPrefix(command, prefix) {
			return execution.CommandTest
		}
	}
	for _, prefix := range []string{"go build", "npm run build", "bun run build", "cargo build", "mvn package", "make build"} {
		if strings.HasPrefix(command, prefix) {
			return execution.CommandBuild
		}
	}
	for _, prefix := range []string{"go vet", "golangci-lint", "npm run lint", "bun run lint", "tsc", "make lint"} {
		if strings.HasPrefix(command, prefix) {
			return execution.CommandDiagnostics
		}
	}
	if strings.HasPrefix(command, "git diff") {
		return execution.CommandDiff
	}
	for _, prefix := range []string{"git status", "git log", "git show", "pwd", "ls", "rg "} {
		if strings.HasPrefix(command, prefix) {
			return execution.CommandRead
		}
	}
	return execution.CommandUnknown
}
