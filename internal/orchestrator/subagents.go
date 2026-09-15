package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/harness"
)

const (
	configuredSubagentTimeout = 30 * time.Minute
	subagentTaskLimit         = 64 << 10
	subagentResultLimit       = 256 << 10
	subagentPreviewLimit      = 4 << 10
)

var errSubagentBusy = errors.New("subagent busy: nested delegation cannot wait for capacity or an active agent")

// subagentSizeError retains only a small, detached preview, never the full result.
type subagentSizeError struct {
	Resource string
	Size     int
	Limit    int
	Preview  string
}

func (e *subagentSizeError) Error() string {
	message := fmt.Sprintf("subagent %s exceeds byte limit: %d > %d", e.Resource, e.Size, e.Limit)
	if e.Preview != "" {
		message += "; truncated preview: " + e.Preview
	}
	return message
}

type subagentTurnKey struct{}
type subagentPathKey struct{}
type subagentActiveKey struct{}

type subagentTurnState struct {
	modelCalls atomic.Int32
	maxCalls   int32
}

func withSubagentTurnState(ctx context.Context, maxModelCalls int, rootAgent string) context.Context {
	if ctx.Value(subagentTurnKey{}) != nil {
		return ctx
	}
	ctx = context.WithValue(ctx, subagentTurnKey{}, &subagentTurnState{maxCalls: int32(maxModelCalls)})
	if rootAgent != "" {
		ctx = context.WithValue(ctx, subagentPathKey{}, []string{rootAgent})
	}
	return ctx
}

func claimTurnModelCall(ctx context.Context) error {
	state, _ := ctx.Value(subagentTurnKey{}).(*subagentTurnState)
	if state == nil || state.maxCalls <= 0 {
		return nil
	}
	if calls := state.modelCalls.Add(1); calls > state.maxCalls {
		return fmt.Errorf("model call limit exceeded: maximum %d per turn", state.maxCalls)
	}
	return nil
}

type configuredAgentRunner struct {
	agents    map[string]*agent.Agent
	fallback  harness.Runner
	resources *subagentResources
	once      sync.Once
}

// One resource set is shared by every parent runner in an orchestrator. Each
// active request (including descendants and dynamic agents) holds a permit.
type subagentResources struct {
	gate   chan struct{}
	agents map[*agent.Agent]chan struct{}
}

func newSubagentResources(agents map[string]*agent.Agent) *subagentResources {
	limit := 0
	locks := make(map[*agent.Agent]chan struct{}, len(agents))
	for _, a := range agents {
		if a == nil {
			continue
		}
		n := a.MaxConcurrentSubAgents
		if n <= 0 {
			n = 5 // SDK builder default; an unset limit must still be finite.
		}
		if limit == 0 || n < limit {
			limit = n
		}
		locks[a] = make(chan struct{}, 1)
	}
	if limit == 0 {
		limit = 5
	}
	return &subagentResources{gate: make(chan struct{}, limit), agents: locks}
}

// Nested requests must not queue: ancestors hold both capacity and agent leases
// while awaiting them. This also avoids cross-root A -> B / B -> A lock cycles.
func acquireSubagentLease(ctx context.Context, gate chan struct{}, nested bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if nested {
		select {
		case gate <- struct{}{}:
		default:
			return errSubagentBusy
		}
	} else {
		select {
		case gate <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		<-gate
		return err
	}
	return nil
}

func (r *configuredAgentRunner) Run(ctx context.Context, spec harness.SubAgentSpec, task string) (string, error) {
	// Include dynamic prompts and tool names so the fallback cannot bypass the
	// input bound. Reject rather than silently discarding task context.
	size := len(task) + len(spec.Name) + len(spec.Description) + len(spec.SystemPrompt)
	for _, instruction := range spec.Instructions {
		size += len(instruction)
	}
	for _, name := range spec.ToolNames {
		size += len(name)
	}
	if size > subagentTaskLimit {
		return "", &subagentSizeError{Resource: "task/spec", Size: size, Limit: subagentTaskLimit}
	}
	path, _ := ctx.Value(subagentPathKey{}).([]string)
	for _, agentID := range path {
		if agentID == spec.Name {
			return "", fmt.Errorf("subagent delegation cycle detected: %v -> %s", path, spec.Name)
		}
	}
	r.once.Do(func() {
		if r.resources == nil {
			r.resources = newSubagentResources(r.agents)
		}
	})
	runCtx, cancel := context.WithTimeout(ctx, configuredSubagentTimeout)
	defer cancel()
	nested, _ := ctx.Value(subagentActiveKey{}).(bool)
	if err := acquireSubagentLease(runCtx, r.resources.gate, nested); err != nil {
		return "", fmt.Errorf("subagent capacity: %w", err)
	}
	defer func() { <-r.resources.gate }()
	nextPath := append(append([]string(nil), path...), spec.Name)
	runCtx = context.WithValue(runCtx, subagentPathKey{}, nextPath)
	runCtx = context.WithValue(runCtx, subagentActiveKey{}, true)
	var result string
	var err error
	if configured := r.agents[spec.Name]; configured != nil {
		lease := r.resources.agents[configured]
		if err := acquireSubagentLease(runCtx, lease, nested); err != nil {
			return "", fmt.Errorf("configured subagent %q: %w", spec.Name, err)
		}
		defer func() { <-lease }()
		// Execute builds request-local conversation state. Retain the complete
		// configured object (security, skills, hooks and services), rather than
		// copying Agent or maintaining an incomplete builder field allowlist.
		// Runtime configuration must still change only between active runs.
		result, err = configured.Execute(runCtx, task)
	} else if r.fallback != nil {
		result, err = r.fallback.Run(runCtx, spec, task)
	} else {
		err = fmt.Errorf("no dynamic subagent runner configured")
	}
	if runCtx.Err() != nil {
		return "", fmt.Errorf("subagent %q: %w", spec.Name, runCtx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("subagent %q: %w", spec.Name, err)
	}
	if len(result) > subagentResultLimit {
		end := subagentPreviewLimit
		for end > 0 && !utf8.RuneStart(result[end]) {
			end--
		}
		return "", &subagentSizeError{
			Resource: "result", Size: len(result), Limit: subagentResultLimit,
			Preview: strings.Clone(result[:end]),
		}
	}
	return result, nil
}

// setupSubAgents makes every configured peer available as a real delegation
// target while retaining dynamic subagents through the standard harness runner.
func setupSubAgents(agents map[string]*agent.Agent) error {
	resources := newSubagentResources(agents)
	for parentID, parent := range agents {
		svc, err := harness.NewSubAgentService(parent)
		if err != nil {
			return fmt.Errorf("configure subagents for %q: %w", parentID, err)
		}
		for childID, child := range agents {
			if childID == parentID {
				continue
			}
			if err := svc.Register(harness.SubAgentSpec{
				Name:        childID,
				Description: child.Description,
			}); err != nil {
				return fmt.Errorf("register subagent %q for %q: %w", childID, parentID, err)
			}
		}
		harness.Attach(svc, &configuredAgentRunner{
			agents:    agents,
			fallback:  harness.NewInProcessRunner(svc),
			resources: resources,
		})
	}
	return nil
}
