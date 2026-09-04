package security

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	chronossandbox "github.com/spawn08/chronos/sandbox"
)

const (
	macOSSandboxHelper    = "sandbox-exec"
	linuxSandboxHelper    = "bwrap"
	defaultSandboxTimeout = 2 * time.Minute
)

// OSSandbox executes commands using the host's mandatory OS sandbox helper.
type OSSandbox struct {
	workspace    string
	helper       string
	platform     string
	allowNetwork bool
}

var _ chronossandbox.Sandbox = (*OSSandbox)(nil)

// NewOSSandbox creates a fail-closed sandbox rooted at workspace. Network
// access is disabled unless allowNetwork is explicitly true.
func NewOSSandbox(workspace string, allowNetwork bool) (*OSSandbox, error) {
	return newOSSandbox(workspace, allowNetwork, runtime.GOOS, exec.LookPath)
}

func newOSSandbox(workspace string, allowNetwork bool, platform string, lookPath func(string) (string, error)) (*OSSandbox, error) {
	canonical, err := canonicalWorkspace(workspace)
	if err != nil {
		return nil, err
	}

	helperName := ""
	switch platform {
	case "darwin":
		helperName = macOSSandboxHelper
	case "linux":
		helperName = linuxSandboxHelper
	default:
		return nil, fmt.Errorf("security: OS sandbox unsupported on %s", platform)
	}

	helper, err := lookPath(helperName)
	if err != nil {
		return nil, fmt.Errorf("security: required sandbox helper %q unavailable: %w", helperName, err)
	}

	return &OSSandbox{
		workspace:    canonical,
		helper:       helper,
		platform:     platform,
		allowNetwork: allowNetwork,
	}, nil
}

func canonicalWorkspace(workspace string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("security: sandbox workspace is required")
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("security: resolve sandbox workspace: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("security: canonicalize sandbox workspace: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("security: stat sandbox workspace: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("security: sandbox workspace %q is not a directory", canonical)
	}
	if !utf8.ValidString(canonical) || strings.IndexFunc(canonical, func(r rune) bool { return r < ' ' }) >= 0 {
		return "", fmt.Errorf("security: sandbox workspace contains an unsafe character")
	}
	return filepath.Clean(canonical), nil
}

// Execute implements sandbox.Sandbox.
func (s *OSSandbox) Execute(ctx context.Context, command string, args []string, timeout time.Duration) (*chronossandbox.Result, error) {
	if timeout <= 0 {
		timeout = defaultSandboxTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	helperArgs, err := s.commandArgs(command, args)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, s.helper, helperArgs...)
	cmd.Dir = s.workspace

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	result := &chronossandbox.Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, fmt.Errorf("security: sandbox execution canceled: %w", ctxErr)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("security: execute sandbox helper: %w", err)
	}
	return result, nil
}

func (s *OSSandbox) commandArgs(command string, args []string) ([]string, error) {
	if command == "" {
		return nil, fmt.Errorf("security: sandbox command is required")
	}

	switch s.platform {
	case "darwin":
		profile := macOSSandboxProfile(s.workspace, s.allowNetwork)
		result := []string{"-p", profile, "--", command}
		return append(result, args...), nil
	case "linux":
		result := []string{"--die-with-parent", "--new-session", "--unshare-all"}
		if s.allowNetwork {
			result = append(result, "--share-net")
		}
		result = append(result,
			"--ro-bind", "/", "/",
			"--dev", "/dev",
			"--proc", "/proc",
			"--bind", s.workspace, s.workspace,
			"--chdir", s.workspace,
			"--", command,
		)
		return append(result, args...), nil
	default:
		return nil, fmt.Errorf("security: OS sandbox unsupported on %s", s.platform)
	}
}

func macOSSandboxProfile(workspace string, allowNetwork bool) string {
	quotedWorkspace := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(workspace) + `"`
	lines := []string{
		"(version 1)",
		"(deny default)",
		"(allow process*)",
		"(allow signal (target self))",
		"(allow file-read*)",
		"(allow file-write* (subpath " + quotedWorkspace + "))",
		"(allow sysctl-read)",
		"(allow mach-lookup)",
	}
	if allowNetwork {
		lines = append(lines, "(allow network*)")
	}
	return strings.Join(lines, "\n")
}

// Close implements sandbox.Sandbox. The backend owns no persistent resources.
func (s *OSSandbox) Close() error { return nil }
