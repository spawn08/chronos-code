package tui

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/storage"
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
	all    bool
}

type approvalCache struct {
	mu       sync.Mutex
	tools    map[string]bool
	sessions map[string]bool
}

func newApprovalCache() *approvalCache {
	return &approvalCache{tools: make(map[string]bool), sessions: make(map[string]bool)}
}

func approvalKey(sessionID, toolName string) string { return sessionID + "\x00" + toolName }

func (c *approvalCache) allowed(sessionID, toolName string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[sessionID] || c.tools[approvalKey(sessionID, toolName)]
}

func (c *approvalCache) remember(sessionID, toolName string, decision approvalDecision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if decision.all {
		c.sessions[sessionID] = true
	} else if decision.always {
		c.tools[approvalKey(sessionID, toolName)] = true
	}
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
	cache := newApprovalCache()

	return func(ctx context.Context, toolName string, args map[string]any) (bool, error) {
		sessionID := storage.SessionFromContext(ctx)
		if cache.allowed(sessionID, toolName) {
			return true, nil
		}

		resp := make(chan approvalDecision, 1)
		p.Send(approvalRequestMsg{toolName: toolName, args: args, resp: resp})

		select {
		case dec := <-resp:
			if dec.allow {
				cache.remember(sessionID, toolName, dec)
			}
			return dec.allow, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}
