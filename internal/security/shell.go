package security

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
)

// NewWorkspaceShellTool executes through a shell rooted in workspace and
// terminates the complete process group when the context or timeout expires.
func NewWorkspaceShellTool(workspace string, timeout time.Duration) *tool.Definition {
	return NewWorkspaceShellToolWithPolicy(workspace, timeout, nil)
}

// NewWorkspaceShellToolWithPolicy is NewWorkspaceShellTool that also accepts
// working directories the user approved for this session on policy.
func NewWorkspaceShellToolWithPolicy(workspace string, timeout time.Duration, policy *Policy) *tool.Definition {
	if timeout <= 0 {
		timeout = defaultSandboxTimeout
	}
	return &tool.Definition{
		Name:        "shell",
		Description: "Execute a shell command in the workspace and return stdout/stderr.",
		Permission:  tool.PermRequireApproval,
		Effects: []tool.Effect{
			tool.EffectRead, tool.EffectDeliveryWrite, tool.EffectProcessExecution,
			tool.EffectNetwork, tool.EffectExternalMutation,
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":     map[string]any{"type": "string"},
				"working_dir": map[string]any{"type": "string"},
			},
			"required": []string{"command"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			command, _ := args["command"].(string)
			if command == "" {
				return nil, fmt.Errorf("shell: 'command' argument is required")
			}
			dir, err := workspaceDirectoryWithPolicy(builtins.WorkspaceRoot(ctx, workspace), args["working_dir"], policy)
			if err != nil {
				return nil, err
			}
			if policy, mandatory := mandatorySandbox(ctx); mandatory {
				if _, err := workspaceDirectory(builtins.WorkspaceRoot(ctx, workspace), dir); err != nil {
					return nil, fmt.Errorf("shell: mandatory sandbox only mounts the workspace: %w", err)
				}
				sandbox, err := NewContainerShellSandbox(ctx, builtins.WorkspaceRoot(ctx, workspace), policy)
				if err != nil {
					return nil, fmt.Errorf("shell: mandatory sandbox: %w", err)
				}
				defer sandbox.Close()
				relative, err := filepath.Rel(sandbox.Workspace, dir)
				if err != nil {
					return nil, fmt.Errorf("shell: sandbox working directory: %w", err)
				}
				sandbox.Container.WorkingDir = filepath.Join("/workspace", relative)
				result, err := sandbox.Container.Execute(ctx, "/bin/sh", []string{"-c", command}, timeout)
				if err != nil {
					return nil, fmt.Errorf("shell: sandbox execution: %w", err)
				}
				return map[string]any{"stdout": result.Stdout, "stderr": result.Stderr, "exit_code": result.ExitCode}, nil
			}
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.Command("sh", "-c", command)
			cmd.Dir = dir
			configureProcessGroup(cmd)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Start(); err != nil {
				return nil, fmt.Errorf("shell: %w", err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var waitErr error
			select {
			case waitErr = <-done:
			case <-runCtx.Done():
				killProcessGroup(cmd)
				waitErr = <-done
				return map[string]any{"stdout": stdout.String(), "stderr": stderr.String(), "exit_code": exitCode(waitErr)}, fmt.Errorf("shell: %w", runCtx.Err())
			}
			return map[string]any{"stdout": stdout.String(), "stderr": stderr.String(), "exit_code": exitCode(waitErr)}, nil
		},
	}
}

// workspaceDirectoryWithPolicy resolves requested like workspaceDirectory but
// also accepts directories granted read/write for the session on policy.
func workspaceDirectoryWithPolicy(workspace string, requested any, policy *Policy) (string, error) {
	dir, err := workspaceDirectory(workspace, requested)
	if err == nil || policy == nil {
		return dir, err
	}
	resolved, resolveErr := resolveShellDir(workspace, requested)
	if resolveErr != nil {
		return "", err
	}
	if policy.sessionDirectoryAllows(resolved, DirectoryWrite) {
		return resolved, nil
	}
	return "", err
}

// resolveShellDir returns the canonical absolute working directory requested
// relative to workspace, without enforcing any boundary.
func resolveShellDir(workspace string, requested any) (string, error) {
	dir := workspace
	if value, ok := requested.(string); ok && value != "" {
		dir = value
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(workspace, dir)
		}
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(dir)
}

func workspaceDirectory(workspace string, requested any) (string, error) {
	dir := workspace
	if value, ok := requested.(string); ok && value != "" {
		dir = value
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(workspace, dir)
		}
	}
	root, err := canonicalWorkspace(workspace)
	if err != nil {
		return "", fmt.Errorf("shell: resolve workspace: %w", err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("shell: resolve working directory: %w", err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("shell: resolve working directory symlinks: %w", err)
	}
	relative, err := filepath.Rel(root, dir)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || len(relative) > 3 && relative[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("shell: working directory is outside workspace")
	}
	return dir, nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
