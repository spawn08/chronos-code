package tui

import (
	"context"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spawn08/chronos/engine/tool"
)

// approvalRequestMsg asks the running Program to show a permission modal for
// a tool call. It is sent from the agent's tool-execution goroutine (see
// NewApprovalHandler below), never from the Update loop itself, so the
// modal's answer must come back over resp rather than a direct return value.
type approvalRequestMsg struct {
	toolName string
	args     map[string]any
	resp     chan approvalDecision
}

type approvalDecision struct {
	allow  bool
	always bool
}

// NewApprovalHandler returns a tool.ApprovalFunc (engine/tool/registry.go)
// that asks the given running Program to render a permission modal and
// blocks until the user answers. Bubbletea already owns stdin for its own
// key events, so — unlike the old REPL's InteractiveApproval — this cannot
// read from a second bufio.Reader on stdin; that would race bubbletea for the
// same fd. tea.Program.Send is the documented concurrency-safe bridge
// instead: it's called from whatever goroutine is executing the tool call,
// while Update (on the main event-loop goroutine) resolves resp once the user
// presses y/n/a.
func NewApprovalHandler(p *tea.Program) tool.ApprovalFunc {
	var mu sync.Mutex
	autoApproved := make(map[string]bool)

	return func(ctx context.Context, toolName string, args map[string]any) (bool, error) {
		mu.Lock()
		approved := autoApproved[toolName]
		mu.Unlock()
		if approved {
			return true, nil
		}

		resp := make(chan approvalDecision, 1)
		p.Send(approvalRequestMsg{toolName: toolName, args: args, resp: resp})

		select {
		case dec := <-resp:
			if dec.always {
				mu.Lock()
				autoApproved[toolName] = true
				mu.Unlock()
			}
			return dec.allow, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}
