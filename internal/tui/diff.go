package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type diffSnapshotMsg struct {
	id      uint64
	content string
	err     error
}

type diffOutput struct {
	text      strings.Builder
	truncated bool
}

func (out *diffOutput) Write(p []byte) (int, error) {
	n := len(p)
	const limit = maxInspectionBytes - 2048
	if remaining := limit - out.text.Len(); remaining > 0 {
		out.text.Write(p[:min(n, remaining)])
	}
	if out.text.Len() >= limit {
		out.truncated = true
	}
	return n, nil
}

func runGitDiff(ctx context.Context, dir string, out *diffOutput, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Stdout = out
	var stderr diffOutput
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.text.String()))
	}
	return nil
}

func workspaceDiff(ctx context.Context, dir string) (string, error) {
	var out diffOutput
	if err := runGitDiff(ctx, dir, &out, "diff", "--no-ext-diff", "--no-color", "HEAD", "--"); err != nil {
		return "", err
	}
	if !out.truncated {
		var untracked diffOutput
		if err := runGitDiff(ctx, dir, &untracked, "ls-files", "--others", "--exclude-standard", "-z"); err != nil {
			return "", err
		}
		paths := bytes.Split([]byte(untracked.text.String()), []byte{0})
		if untracked.truncated {
			paths = paths[:len(paths)-1] // The last pathname may be incomplete.
		}
		for i, path := range paths {
			if len(path) == 0 {
				continue
			}
			if out.truncated || i >= 100 {
				out.truncated = true
				break
			}
			cmd := exec.CommandContext(ctx, "git", "diff", "--no-ext-diff", "--no-color", "--no-index", "--", "/dev/null", "./"+string(path))
			cmd.Dir = dir
			cmd.Stdout = &out
			var stderr diffOutput
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					return "", fmt.Errorf("git diff untracked %s: %w: %s", path, err, strings.TrimSpace(stderr.text.String()))
				}
			}
		}
		if untracked.truncated {
			out.truncated = true
		}
	}
	content := out.text.String()
	if content == "" {
		content = "No workspace changes."
	}
	if out.truncated {
		content = limitInspection(content) + "\n[diff snapshot truncated; use git diff for the remainder]"
	}
	return "Read-only workspace snapshot (includes existing staged, unstaged, and untracked changes).\n\n" + content, nil
}

func (m *appModel) openWorkspaceDiff() tea.Cmd {
	m.diffRequestID++
	id, ctx, dir := m.diffRequestID, m.ctx, m.workspaceRoot()
	return func() tea.Msg {
		content, err := workspaceDiff(ctx, dir)
		return diffSnapshotMsg{id: id, content: content, err: err}
	}
}
