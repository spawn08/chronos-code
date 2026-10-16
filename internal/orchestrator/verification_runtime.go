package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/spawn08/chronos/engine/model"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/claims"
	"github.com/spawn08/chronos-code/internal/checks"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/security"
)

type taskRuntimeKey struct{}

// taskRuntime owns the immutable identity and evidence ledger for one
// top-level execution. Child contexts inherit it unless they explicitly start
// another task.
type taskRuntime struct {
	taskID        execution.TaskID
	workspaceRoot string
	ledger        *execution.Ledger
	claims        *claims.Store
	// claimsPath is the durable claims snapshot; empty keeps claims in memory.
	claimsPath       string
	claimsSaveMu     sync.Mutex
	budget           *execution.TaskBudget
	usageMu          sync.Mutex
	usage            model.Usage
	costMicrodollars int64
	// refusesShell reports whether the security policy refuses a shell
	// command outright; nil refuses nothing.
	refusesShell func(command string) bool
	// background maps a running background shell to its original call, so
	// its exit is recorded as evidence for the command that started it.
	backgroundMu   sync.Mutex
	background     map[string]backgroundCall
	sourceMu       sync.Mutex
	sources        map[string]string
	knowledgeMu    sync.Mutex
	knowledge      []readKnowledge
	observationCtx context.Context
	backgroundShells *security.BackgroundShells
}

type backgroundCall struct {
	args     map[string]any
	started  time.Time
	revision uint64
}

func (r *taskRuntime) trackBackground(id string, args map[string]any, started time.Time, revision uint64) {
	r.backgroundMu.Lock()
	defer r.backgroundMu.Unlock()
	if r.background == nil {
		r.background = map[string]backgroundCall{}
	}
	r.background[id] = backgroundCall{args: args, started: started, revision: revision}
}

// finishBackground returns and forgets the call that started shell id.
func (r *taskRuntime) finishBackground(id string) (map[string]any, time.Time, uint64, bool) {
	r.backgroundMu.Lock()
	defer r.backgroundMu.Unlock()
	call, ok := r.background[id]
	delete(r.background, id)
	return call.args, call.started, call.revision, ok
}

func newTaskRuntime(taskID, workspaceRoot string) (*taskRuntime, error) {
	return newTaskRuntimeWithLimits(taskID, workspaceRoot, execution.TaskLimits{})
}

func newTaskRuntimeWithLimits(taskID, workspaceRoot string, limits execution.TaskLimits) (*taskRuntime, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task runtime: task ID is required")
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("task runtime: resolve workspace root: %w", err)
	}
	store, err := claims.NewStore(root)
	if err != nil {
		return nil, fmt.Errorf("task runtime: %w", err)
	}
	return &taskRuntime{
		taskID:        execution.TaskID(taskID),
		workspaceRoot: root,
		ledger:        execution.NewLedger(execution.TaskID(taskID)),
		claims:        store,
		budget:        execution.NewTaskBudget(limits, time.Now()),
	}, nil
}

func taskLimits(cfg *config.Config) execution.TaskLimits {
	if cfg == nil {
		return execution.TaskLimits{}
	}
	return execution.TaskLimits{
		RepairAttempts:   cfg.Repair.MaxAttempts,
		ModelCalls:       cfg.Repair.MaxModelCalls,
		ToolCalls:        cfg.Repair.MaxToolCalls,
		WallTime:         time.Duration(cfg.Repair.WallTimeSec) * time.Second,
		Tokens:           cfg.Repair.MaxTokens,
		CostMicrodollars: cfg.Repair.MaxCostMicrodollars,
	}
}

func (r *taskRuntime) beforeModelCall() error {
	return r.budget.ConsumeModelCall()
}

func (r *taskRuntime) beforeToolCall() error {
	return r.budget.ConsumeToolCall()
}

func (r *taskRuntime) recordModelUsage(modelID string, usage model.Usage) error {
	tokens := int64(usage.UncachedPromptTokens() + usage.CacheCreationTokens + usage.CompletionTokens)
	var cost int64
	price, priceErr := budget.PriceForModel(modelID)
	if priceErr == nil {
		incurred, err := price.IncurredCost(usage)
		if err != nil {
			return fmt.Errorf("price model usage: %w", err)
		}
		cost = int64(incurred)
	} else if r.budget.Snapshot().Limits.CostMicrodollars > 0 {
		return priceErr
	}
	r.usageMu.Lock()
	r.usage.Add(usage)
	r.costMicrodollars += cost
	r.usageMu.Unlock()
	return r.budget.AddUsage(tokens, cost)
}

func withTaskRuntime(ctx context.Context, runtime *taskRuntime) context.Context {
	if runtime == nil {
		return ctx
	}
	return context.WithValue(ctx, taskRuntimeKey{}, runtime)
}

func taskRuntimeFromContext(ctx context.Context) (*taskRuntime, bool) {
	runtime, ok := ctx.Value(taskRuntimeKey{}).(*taskRuntime)
	return runtime, ok && runtime != nil
}

func (r *taskRuntime) recordWrite(path, contentHash string, sizeBytes int64, provenance execution.EvidenceProvenance, completedAt time.Time) (execution.Event, error) {
	scope, err := execution.NormalizeScope(r.workspaceRoot, execution.Scope{Kind: execution.ScopeExact, Path: path})
	if err != nil {
		return execution.Event{}, err
	}
	return r.ledger.Record(execution.Event{
		Type:        execution.EventWrite,
		Scopes:      []execution.Scope{scope},
		Paths:       []string{scope.Path},
		ContentHash: contentHash,
		SizeBytes:   sizeBytes,
		Provenance:  provenance,
		CompletedAt: completedAt,
	})
}

func (r *taskRuntime) recordWorkspaceMutation(detail string, completedAt time.Time) (execution.Event, error) {
	return r.ledger.Record(execution.Event{
		Type:        execution.EventWrite,
		Scopes:      []execution.Scope{{Kind: execution.ScopeWorkspace}},
		Paths:       []string{"."},
		Detail:      detail,
		Provenance:  execution.ProvenanceRuntime,
		CompletedAt: completedAt,
	})
}

func (r *taskRuntime) recordCommand(command string, class execution.CommandClass, exitCode *int, terminal execution.TerminalState, scopes []execution.Scope, provenance execution.EvidenceProvenance, startedAt, completedAt time.Time) (execution.Event, error) {
	return r.recordCommandAt(r.workspaceRoot, command, class, exitCode, terminal, scopes, provenance, startedAt, completedAt)
}

func (r *taskRuntime) recordCommandAt(directory, command string, class execution.CommandClass, exitCode *int, terminal execution.TerminalState, scopes []execution.Scope, provenance execution.EvidenceProvenance, startedAt, completedAt time.Time, revision ...uint64) (execution.Event, error) {
	ctx := r.observationCtx
	if ctx == nil {
		ctx = context.Background()
	}
	inferred, err := checks.Infer(ctx, r.workspaceRoot, directory, command)
	if err != nil {
		return execution.Event{}, fmt.Errorf("infer verification coverage: %w", err)
	}
	if len(scopes) == 0 {
		if !inferred.Known || len(inferred.Scopes) == 0 {
			return r.ledger.Record(execution.Event{Type: execution.EventUncertainty, Command: command, CommandClass: class, Detail: "verification coverage unknown: " + command, Provenance: provenance, StartedAt: startedAt, CompletedAt: completedAt})
		}
		scopes = inferred.Scopes
	}
	normalized := make([]execution.Scope, 0, len(scopes))
	paths := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		normalizedScope, err := execution.NormalizeScope(r.workspaceRoot, scope)
		if err != nil {
			return execution.Event{}, err
		}
		normalized = append(normalized, normalizedScope)
		if normalizedScope.Kind == execution.ScopeWorkspace {
			paths = append(paths, ".")
		} else if normalizedScope.Path != "" {
			paths = append(paths, normalizedScope.Path)
		}
	}
	if len(revision) > 0 {
		state, err := r.ledger.State()
		if err != nil {
			return execution.Event{}, err
		}
		for _, write := range state.Writes {
			writeScopes := append([]execution.Scope(nil), write.Scopes...)
			if len(writeScopes) == 0 {
				for _, path := range write.Paths {
					writeScopes = append(writeScopes, execution.Scope{Kind: execution.ScopeDirectory, Path: path})
				}
			}
			if write.MutationRevision > revision[0] && execution.ScopesOverlap(normalized, writeScopes) {
				terminal = execution.TerminalCancelled
				break
			}
		}
	}
	passed := terminal == execution.TerminalExited && exitCode != nil && *exitCode == 0
	return r.ledger.Record(execution.Event{
		Type:          execution.EventVerification,
		Scopes:        normalized,
		Paths:         paths,
		Passed:        passed,
		Detail:        command,
		Command:       command,
		CommandClass:  class,
		CommandFamily: inferred.Family,
		CheckRoot:     inferred.Root,
		ExitCode:      exitCode,
		TerminalState: terminal,
		Provenance:    provenance,
		StartedAt:     startedAt,
		CompletedAt:   completedAt,
	})
}

func (r *taskRuntime) snapshot() (execution.State, error) {
	if err := r.refreshSourceChanges(false); err != nil {
		return execution.State{}, err
	}
	return r.ledger.State()
}

func populateRuntimeResult(result *ExecutionResult, runtime *taskRuntime) {
	if result == nil || runtime == nil {
		return
	}
	runtime.usageMu.Lock()
	result.Usage = runtime.usage
	result.CostMicrodollars = runtime.costMicrodollars
	runtime.usageMu.Unlock()

	state, err := runtime.snapshot()
	if err != nil {
		return
	}
	paths := make(map[string]struct{})
	for _, write := range state.Writes {
		for _, path := range write.Paths {
			paths[path] = struct{}{}
		}
	}
	for path := range paths {
		result.ChangedPaths = append(result.ChangedPaths, path)
	}
	sort.Strings(result.ChangedPaths)
	for id, evidence := range state.Verification {
		if evidence.Current && evidence.Event.Passed {
			result.EvidenceIDs = append(result.EvidenceIDs, id)
		}
	}
	sort.Slice(result.EvidenceIDs, func(i, j int) bool { return result.EvidenceIDs[i] < result.EvidenceIDs[j] })
}
