package orchestrator

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/execution"
)

const (
	// defaultNoProgressWindows applies when long_running.no_progress_windows
	// is unset: one idle window can be exploration, two in a row is a stall.
	defaultNoProgressWindows = 2
	// sessionRenewRatio treats a nearly exhausted session token budget as a
	// full window, renewing it at a round boundary before the budget hook
	// would reject the next model call.
	sessionRenewRatio = 0.9
	// boundedSubagentTimeout is the legacy wall-clock limit for one
	// delegated run outside renew mode.
	boundedSubagentTimeout = 30 * time.Minute
)

// longRunningPolicy is the resolved renewable-window policy for one
// interactive execution and the subagents it delegates to.
type longRunningPolicy struct {
	window            workWindow
	noProgressWindows int
	subagentTimeout   time.Duration
	budget            *budget.Tracker
}

// workWindow sizes one renewable window; zero disables a dimension.
type workWindow struct {
	rounds   int
	calls    int
	duration time.Duration
	tokens   int64
}

type longRunningPolicyKey struct{}

// longRunningPolicy returns the renew-mode policy for an execution. Delivery
// workers keep their own bounded accounting and durable recovery contract, so
// they never receive a renewable policy here.
func (o *Orchestrator) longRunningPolicy(ctx context.Context) (*longRunningPolicy, bool) {
	if o == nil || o.cfg == nil || !o.cfg.LongRunning.Renewing() {
		return nil, false
	}
	if identity, ok := agent.RunIdentityFromContext(ctx); ok && identity.DeliveryID != "" {
		return nil, false
	}
	if _, durable := execution.OperationLeaseFromContext(ctx); durable {
		return nil, false
	}
	cfg := o.cfg.LongRunning
	idle := cfg.NoProgressWindows
	if idle <= 0 {
		idle = defaultNoProgressWindows
	}
	return &longRunningPolicy{
		window: workWindow{
			rounds:   cfg.Window.ToolRounds,
			calls:    cfg.Window.ToolCalls,
			duration: time.Duration(cfg.Window.Seconds) * time.Second,
			tokens:   cfg.Window.Tokens,
		},
		noProgressWindows: idle,
		subagentTimeout:   time.Duration(cfg.SubagentTimeoutSec) * time.Second,
		budget:            o.budget,
	}, true
}

func withLongRunningPolicy(ctx context.Context, policy *longRunningPolicy) context.Context {
	if policy == nil {
		return ctx
	}
	return context.WithValue(ctx, longRunningPolicyKey{}, policy)
}

func longRunningPolicyFromContext(ctx context.Context) (*longRunningPolicy, bool) {
	policy, ok := ctx.Value(longRunningPolicyKey{}).(*longRunningPolicy)
	return policy, ok && policy != nil
}

// renewableTaskLimits drops the task limits that renew mode turns into work
// windows. Repairs continue while each one changes the unmet verification
// set (a repeated set stops the loop, so it stays finite) and each repair
// turn runs under the no-progress governor. An explicit cost ceiling stays
// authoritative.
func renewableTaskLimits(limits execution.TaskLimits) execution.TaskLimits {
	return execution.TaskLimits{CostMicrodollars: limits.CostMicrodollars}
}

// windowGovernor bounds one agent's tool loop by progress instead of a fixed
// iteration count. At every round boundary it tracks the current window;
// when any window dimension fills it checks whether the window produced
// progress, starts a new window, and stops only after noProgressWindows
// consecutive windows without progress.
type windowGovernor struct {
	policy  *longRunningPolicy
	runtime *taskRuntime
	now     func() time.Time

	mu          sync.Mutex
	windowStart time.Time
	rounds      int
	calls       int
	tokens      int64
	newCalls    int
	idle        int
	totalRounds int
	seenCalls   map[string]struct{}
	seenWrites  map[string]struct{}
	seenChecks  map[string]struct{}
}

func newWindowGovernor(policy *longRunningPolicy, runtime *taskRuntime, now func() time.Time) *windowGovernor {
	if now == nil {
		now = time.Now
	}
	g := &windowGovernor{
		policy: policy, runtime: runtime, now: now, windowStart: now(),
		seenCalls: make(map[string]struct{}), seenWrites: make(map[string]struct{}), seenChecks: make(map[string]struct{}),
	}
	// Evidence recorded before this execution (a persisted task ledger) is
	// not progress made by it.
	g.observeEvidence()
	return g
}

// AfterToolRound implements agent.ToolLoopController.
func (g *windowGovernor) AfterToolRound(ctx context.Context, round agent.ToolRound) (agent.ToolLoopAction, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rounds++
	g.totalRounds++
	g.calls += len(round.ToolCalls)
	g.tokens += billedWindowTokens(round.Usage)
	for _, call := range round.ToolCalls {
		fingerprint := call.Name + "\x00" + call.Arguments
		if _, seen := g.seenCalls[fingerprint]; !seen {
			g.seenCalls[fingerprint] = struct{}{}
			g.newCalls++
		}
	}
	sessionID := storage.SessionFromContext(ctx)
	sessionFull := g.policy.budget != nil && sessionID != "" && g.policy.budget.Ratio(sessionID) >= sessionRenewRatio
	if !sessionFull && !g.windowFullLocked() {
		return agent.ToolLoopAction{}, nil
	}
	if g.progressedLocked() {
		g.idle = 0
	} else {
		g.idle++
	}
	g.windowStart, g.rounds, g.calls, g.tokens, g.newCalls = g.now(), 0, 0, 0, 0
	if g.idle >= g.policy.noProgressWindows {
		return agent.ToolLoopAction{Stop: true, Message: g.pauseMessage()}, nil
	}
	if sessionFull {
		// The session budget is a renewable window in renew mode; cumulative
		// usage is still reported by the execution result.
		g.policy.budget.ResetSession(sessionID)
	}
	return agent.ToolLoopAction{}, nil
}

func (g *windowGovernor) windowFullLocked() bool {
	w := g.policy.window
	return (w.rounds > 0 && g.rounds >= w.rounds) ||
		(w.calls > 0 && g.calls >= w.calls) ||
		(w.tokens > 0 && g.tokens >= w.tokens) ||
		(w.duration > 0 && g.now().Sub(g.windowStart) >= w.duration)
}

// progressedLocked reports whether the closing window changed file content,
// produced a verification result not seen before, or mostly issued tool
// calls not seen before (exploration that reads new code counts).
func (g *windowGovernor) progressedLocked() bool {
	progressed := g.observeEvidence()
	if g.newCalls > 0 && g.newCalls*4 >= g.calls {
		progressed = true
	}
	return progressed
}

// observeEvidence records write and verification evidence in the task
// ledger, returning true when any of it is new.
func (g *windowGovernor) observeEvidence() bool {
	if g.runtime == nil {
		return false
	}
	state, err := g.runtime.snapshot()
	if err != nil {
		return false
	}
	found := false
	for _, write := range state.Writes {
		key := strings.Join(write.Paths, "\x00") + "@" + write.ContentHash
		if write.ContentHash == "" {
			key += "#" + write.Detail
		}
		if _, seen := g.seenWrites[key]; !seen {
			g.seenWrites[key] = struct{}{}
			found = true
		}
	}
	for _, event := range state.Events {
		if event.Type != execution.EventVerification {
			continue
		}
		exit := ""
		if event.ExitCode != nil {
			exit = strconv.Itoa(*event.ExitCode)
		}
		key := fmt.Sprintf("%s\x00%t\x00%s\x00%s", event.Command, event.Passed, exit, event.TerminalState)
		if _, seen := g.seenChecks[key]; !seen {
			g.seenChecks[key] = struct{}{}
			found = true
		}
	}
	return found
}

func (g *windowGovernor) pauseMessage() string {
	return fmt.Sprintf("Paused after %d consecutive work windows without progress (%d tool rounds in this run): no new file changes, "+
		"no new verification results, and mostly repeated tool calls. Completed work is kept. Reply with guidance, or say \"continue\" to resume.",
		g.policy.noProgressWindows, g.totalRounds)
}

// billedWindowTokens mirrors the session budget's incremental spend:
// uncached prompt, cache writes, and completion.
func billedWindowTokens(usage model.Usage) int64 {
	return int64(usage.UncachedPromptTokens() + usage.CacheCreationTokens + usage.CompletionTokens)
}
