package worktree

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
)

// Command describes one non-interactive git invocation.
type Command struct {
	Dir   string
	Args  []string
	Stdin []byte
}

// CommandResult preserves stdout and stderr independently, including binary output.
type CommandResult struct {
	Stdout []byte
	Stderr []byte
}

// Runner makes git behavior replaceable in unit tests.
type Runner interface {
	Run(context.Context, Command) (CommandResult, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	args := []string{"--no-optional-locks", "-C", command.Dir}
	args = append(args, command.Args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
	)
	cmd.Stdin = bytes.NewReader(command.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		return result, fmt.Errorf("git %v: %w: %s", command.Args, err, bytes.TrimSpace(result.Stderr))
	}
	return result, nil
}
