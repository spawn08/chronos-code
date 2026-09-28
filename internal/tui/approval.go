package tui

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/orchestrator"
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
	return newApprovalBridge(p).toolApproval
}

// approvalBridge serves tool and plan approvals through one modal queue and
// one decision cache, so approving a plan with auto-accepted edits also
// approves later file_write calls in that session.
type approvalBridge struct {
	p        *tea.Program
	cache    *approvalCache
	promptMu sync.Mutex
}

func newApprovalBridge(p *tea.Program) *approvalBridge {
	return &approvalBridge{p: p, cache: newApprovalCache()}
}

func (b *approvalBridge) toolApproval(ctx context.Context, toolName string, args map[string]any) (bool, error) {
	sessionID := storage.SessionFromContext(ctx)
	if b.cache.allowed(sessionID, toolName) {
		return true, nil
	}
	// Concurrent subagents may request tools simultaneously. Serialize human
	// prompts so one modal cannot overwrite another, then re-check the cache
	// because an earlier "all session" decision may have approved this call.
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	if b.cache.allowed(sessionID, toolName) {
		return true, nil
	}
	dec, err := b.ask(ctx, toolName, args)
	if err != nil {
		return false, err
	}
	if dec.allow {
		b.cache.remember(sessionID, toolName, dec)
	}
	return dec.allow, nil
}

// planApproval shows the plan review modal: y approves, a approves and
// auto-accepts edits for the session, n keeps planning.
func (b *approvalBridge) planApproval(ctx context.Context, plan string) (bool, error) {
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	dec, err := b.ask(ctx, orchestrator.ExitPlanModeToolName, map[string]any{"plan": plan})
	if err != nil {
		return false, err
	}
	if dec.allow && (dec.always || dec.all) {
		b.cache.remember(storage.SessionFromContext(ctx), "file_write", approvalDecision{allow: true, always: true})
	}
	return dec.allow, nil
}

func (b *approvalBridge) ask(ctx context.Context, toolName string, args map[string]any) (approvalDecision, error) {
	resp := make(chan approvalDecision, 1)
	b.p.Send(approvalRequestMsg{toolName: toolName, args: args, resp: resp})
	select {
	case dec := <-resp:
		return dec, nil
	case <-ctx.Done():
		return approvalDecision{}, ctx.Err()
	}
}
