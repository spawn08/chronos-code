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

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/harness"
	"github.com/spawn08/chronos/storage"
)

const (
	subagentTaskLimit    = 64 << 10
	subagentResultLimit  = 256 << 10
	subagentPreviewLimit = 4 << 10
)

var errSubagentBusy = errors.New("subagent busy: nested delegation cannot wait for capacity or an active agent")

// subagentSizeError retains only a small, detached preview, never the full result.
type subagentSizeError struct {
	Resource string
	Size     int
	Limit    int
	Preview  string
	Artifact string
}

func (e *subagentSizeError) Error() string {
	message := fmt.Sprintf("subagent %s exceeds byte limit: %d > %d", e.Resource, e.Size, e.Limit)
	if e.Preview != "" {
		message += "; truncated preview: " + e.Preview
	}
	if e.Artifact != "" {
		message += "; recover full report with read_artifact artifact_id=" + e.Artifact
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
	tools     *tool.Registry
	models    *roleModelRegistry
	fallback  harness.Runner
	store     storage.Storage
	resources *subagentResources
	once      sync.Once
}

type roleModelRegistry struct {
	mu        sync.RWMutex
	providers map[string]model.Provider
	// route, when set, selects a role's model for one delegated task (nil
	// keeps the role's configured model).
	route func(ctx context.Context, roleID, task string) model.Provider
}

func newRoleModelRegistry(agents map[string]*agent.Agent) *roleModelRegistry {
	providers := make(map[string]model.Provider, len(agents))
	for id, configured := range agents {
		if configured != nil {
			providers[id] = configured.Model
		}
	}
	return &roleModelRegistry{providers: providers}
}

func (r *roleModelRegistry) provider(roleID string) model.Provider {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[roleID]
}

// providerFor is the model for one delegated task of roleID: the routed model
// when routing selects one, else the role's model.
func (r *roleModelRegistry) providerFor(ctx context.Context, roleID, task string) model.Provider {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	route := r.route
	r.mu.RUnlock()
	if route != nil {
		if routed := route(ctx, roleID, task); routed != nil {
			return routed
		}
	}
	return r.provider(roleID)
}

func (r *roleModelRegistry) setRoute(route func(ctx context.Context, roleID, task string) model.Provider) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.route = route
}

func (r *roleModelRegistry) set(roleID string, provider model.Provider) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[roleID] = provider
}

// One resource set is shared by every parent runner in an orchestrator. Each
// active request (including descendants and dynamic agents) holds a permit.
type subagentResources struct {
	gate chan struct{}
}

func newSubagentResources(agents map[string]*agent.Agent) *subagentResources {
	limit := 0
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
	}
	if limit == 0 {
		limit = 5
	}
	return &subagentResources{gate: make(chan struct{}, limit)}
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
	longRunning, renewing := longRunningPolicyFromContext(ctx)
	var runCtx context.Context
	var cancel context.CancelFunc
	switch {
	case !renewing:
		runCtx, cancel = context.WithTimeout(ctx, boundedSubagentTimeout)
	case longRunning.subagentTimeout > 0:
		runCtx, cancel = context.WithTimeout(ctx, longRunning.subagentTimeout)
	default:
		runCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	nested, _ := ctx.Value(subagentActiveKey{}).(bool)
	if err := acquireSubagentLease(runCtx, r.resources.gate, nested); err != nil {
		return "", fmt.Errorf("subagent capacity: %w", err)
	}
	defer func() { <-r.resources.gate }()
	nextPath := append(append([]string(nil), path...), spec.Name)
	runCtx = context.WithValue(runCtx, subagentPathKey{}, nextPath)
	runCtx = context.WithValue(runCtx, subagentActiveKey{}, true)
	// Relevance-ranked context (memory, diagnostics, learned patterns) is
	// chosen for the delegated task, not the parent's user message.
	runCtx = context.WithValue(runCtx, messageKey{}, task)
	requestedTask := task
	if runtime, ok := taskRuntimeFromContext(runCtx); ok {
		task += runtime.boundedHandoff(task, subagentTaskLimit-size)
	}
	var result string
	var err error
	if configured := r.agents[spec.Name]; configured != nil {
		runCtx = attenuateEffectGrant(runCtx, configured.Tools.List())
		provider := configured.Model
		if r.models != nil {
			provider = r.models.providerFor(runCtx, spec.Name, requestedTask)
		}
		if provider == nil {
			return "", fmt.Errorf("configured subagent %q has no model", spec.Name)
		}
		// Execute builds request-local conversation state. Retain the complete
		// configured object (security, skills, hooks and services), rather than
		// copying Agent or maintaining an incomplete builder field allowlist.
		// Runtime configuration must still change only between active runs.
		identity, _ := agent.RunIdentityFromContext(runCtx)
		identity.ParentInvocationID = identity.InvocationID
		identity.InvocationID = session.NewSessionID()
		identity.RoleID = configured.ID
		runCtx = agent.WithRunIdentity(runCtx, identity)
		runCtx = agent.WithModelProvider(runCtx, provider)
		if renewing {
			// The child gets its own windows and progress history; the
			// parent's governor waits at its round boundary meanwhile.
			runtime, _ := taskRuntimeFromContext(runCtx)
			runCtx = agent.WithToolLoopController(runCtx, configured.ID, newWindowGovernor(longRunning, runtime, time.Now))
		}
		if inbox, ok := runCtx.Value(supervisedInboxKey{}).(*SteeringInbox); ok {
			runCtx = agent.WithPendingInput(runCtx, configured.ID, inbox)
		}
		// Durable delivery relies on the blocking path's reply journal; other
		// providers keep their existing blocking behavior.
		if _, durable := execution.OperationLeaseFromContext(runCtx); !durable && anthropicMessagesProvider(provider) {
			result, err = executeSubagentStream(runCtx, configured, task)
		} else {
			result, err = configured.Execute(runCtx, task)
		}
	} else if r.fallback != nil {
		if _, durable := execution.OperationLeaseFromContext(runCtx); durable {
			return "", fmt.Errorf("dynamic delivery subagent has no durable usage hook: %w", execution.ErrUsageOutcomeUnknown)
		}
		runCtx = attenuateDynamicEffectGrant(runCtx, spec, r.tools)
		result, err = r.fallback.Run(runCtx, spec, task)
	} else {
		err = fmt.Errorf("no dynamic subagent runner configured")
	}
	artifactID, artifactErr := r.saveSubagentReport(runCtx, spec.Name, result, err)
	if artifactErr != nil {
		return "", fmt.Errorf("persist subagent %q report: %w", spec.Name, artifactErr)
	}
	recovery := ""
	if artifactID != "" {
		recovery = "\n[Recover full report with read_artifact artifact_id=" + artifactID + "]"
	}
	if runCtx.Err() != nil {
		if strings.TrimSpace(result) != "" {
			partial := partialSubagentReport(result, context.Cause(runCtx))
			return partial + recovery, fmt.Errorf("subagent %q interrupted (partial findings): %w\n%s%s", spec.Name, context.Cause(runCtx), boundedSubagentEvidence(partial, subagentPreviewLimit), recovery)
		}
		return "", fmt.Errorf("subagent %q: %w", spec.Name, context.Cause(runCtx))
	}
	if err != nil {
		if strings.TrimSpace(result) != "" {
			partial := partialSubagentReport(result, err)
			return partial + recovery, fmt.Errorf("subagent %q failed (partial findings): %w\n%s%s", spec.Name, err, boundedSubagentEvidence(partial, subagentPreviewLimit), recovery)
		}
		return "", fmt.Errorf("subagent %q: %w", spec.Name, err)
	}
	if strings.TrimSpace(result) == "" {
		return "", fmt.Errorf("subagent %q: %w", spec.Name, errEmptySubagentResult)
	}
	if len(result) > subagentResultLimit {
		return "", &subagentSizeError{
			Resource: "result", Size: len(result), Limit: subagentResultLimit,
			Preview: boundedSubagentEvidence(result, subagentPreviewLimit), Artifact: artifactID,
		}
	}
	if recovery != "" {
		result = boundedSubagentEvidence(result, subagentResultLimit-len(recovery)) + recovery
	}
	return result, nil
}

func attenuateEffectGrant(ctx context.Context, definitions []*tool.Definition) context.Context {
	parent, constrained := tool.EffectGrantFromContext(ctx)
	if !constrained {
		return ctx
	}
	allowed := make(map[tool.Effect]struct{})
	for _, def := range definitions {
		if def == nil {
			continue
		}
		for _, effect := range def.Effects {
			if _, ok := parent[effect]; ok {
				allowed[effect] = struct{}{}
			}
		}
	}
	effects := make([]tool.Effect, 0, len(allowed))
	for effect := range allowed {
		effects = append(effects, effect)
	}
	return tool.WithEffectGrant(ctx, effects...)
}

func attenuateDynamicEffectGrant(ctx context.Context, spec harness.SubAgentSpec, registry *tool.Registry) context.Context {
	if len(spec.ToolNames) == 0 {
		return tool.WithEffectGrant(ctx)
	}
	definitions := make([]*tool.Definition, 0, len(spec.ToolNames))
	if registry != nil {
		for _, name := range spec.ToolNames {
			if def, ok := registry.Get(name); ok {
				definitions = append(definitions, def)
			}
		}
	}
	return attenuateEffectGrant(ctx, definitions)
}

// setupSubAgents registers only each role's declared children while retaining
// dynamic subagents through the standard harness runner.
func setupSubAgents(agents map[string]*agent.Agent) error {
	return setupSubAgentsWithModels(agents, newRoleModelRegistry(agents))
}

func setupSubAgentsWithModels(agents map[string]*agent.Agent, models *roleModelRegistry) error {
	resources := newSubagentResources(agents)
	for parentID, parent := range agents {
		svc, err := harness.NewSubAgentService(parent)
		if err != nil {
			return fmt.Errorf("configure subagents for %q: %w", parentID, err)
		}
		for _, child := range parent.SubAgents {
			if child == nil || child.ID == "" {
				return fmt.Errorf("configure subagents for %q: declared child is missing an ID", parentID)
			}
			configured, ok := agents[child.ID]
			if !ok || configured != child {
				return fmt.Errorf("configure subagents for %q: declared child %q is not a configured agent", parentID, child.ID)
			}
			if err := svc.Register(harness.SubAgentSpec{
				Name:        child.ID,
				Description: child.Description,
			}); err != nil {
				return fmt.Errorf("register subagent %q for %q: %w", child.ID, parentID, err)
			}
		}
		harness.Attach(svc, &configuredAgentRunner{
			agents:    agents,
			tools:     parent.Tools,
			models:    models,
			fallback:  harness.NewInProcessRunner(svc),
			store:     parent.Storage,
			resources: resources,
		})
	}
	return nil
}

// executeSubagentStream runs a child turn over the streaming path, which is
// not bound by the unary request timeout and receives the raised output
// allowance, so a child can write a large file without truncating mid tool
// call. It returns the child's final message, as agent.Execute does.
func executeSubagentStream(ctx context.Context, a *agent.Agent, task string) (string, error) {
	stream, err := a.ChatStream(ctx, task)
	if err != nil {
		return "", err
	}
	// final is the text after the last tool round, the child's answer.
	// lastNonEmpty keeps the latest earlier text: a child stopped by its
	// output limit right after a tool round would otherwise return "", and
	// its findings were lost and the task re-delegated from scratch.
	var final, lastNonEmpty strings.Builder
	for chunk := range stream {
		if chunk == nil {
			continue
		}
		if chunk.Err != nil {
			go func() {
				for range stream {
				}
			}()
			return latestSubagentText(&final, &lastNonEmpty), chunk.Err
		}
		switch {
		case len(chunk.ToolCalls) > 0:
			if strings.TrimSpace(final.String()) != "" {
				lastNonEmpty.Reset()
				lastNonEmpty.WriteString(final.String())
			}
			final.Reset()
		case chunk.Delta:
			final.WriteString(chunk.Content)
		}
	}
	if err := ctx.Err(); err != nil {
		return latestSubagentText(&final, &lastNonEmpty), err
	}
	if strings.TrimSpace(final.String()) == "" {
		if strings.TrimSpace(lastNonEmpty.String()) != "" {
			return "STATUS: partial (the subagent ended without a final answer; this is its last report)\n\n" + lastNonEmpty.String(), nil
		}
		return "", errEmptySubagentResult
	}
	return final.String(), nil
}

// errEmptySubagentResult reports a child that finished without any answer.
// It is an error, not an empty success, so the parent sees the failure
// instead of treating "" as the work being done.
var errEmptySubagentResult = errors.New("subagent finished without a result (it may have stopped at its output limit); narrow the task or do it directly")

func latestSubagentText(final, previous *strings.Builder) string {
	if strings.TrimSpace(final.String()) != "" {
		return final.String()
	}
	return previous.String()
}

func partialSubagentReport(text string, err error) string {
	return "STATUS: partial (interrupted; " + validPrefix(errorText(err), 256) + ")\n" + boundedSubagentEvidence(text, subagentResultLimit-512)
}

func boundedSubagentEvidence(text string, limit int) string {
	if len(text) <= limit {
		return strings.Clone(text)
	}
	const marker = "\n[... middle omitted; head/tail evidence only ...]\n"
	if limit <= len(marker) {
		return strings.Clone(validPrefix(text, limit))
	}
	budget := limit - len(marker)
	head := validPrefix(text, budget/2)
	start := len(text) - (budget - len(head))
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return head + marker + text[start:]
}
