package orchestrator

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spawn08/chronos/engine/tool"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos-code/internal/worktree"
)

const maxOperationalItems = 8

// OperationalSnapshot is a detached, read-only view of runtime state for UI
// consumers. Every slice is copied before return and can be retained safely.
type OperationalSnapshot struct {
	CapturedAt        time.Time
	PermissionMode    string
	PlanOnly          bool
	VerificationMode  verification.Mode
	Execution         ExecutionSnapshot
	Plan              PlanSnapshot
	ActiveSpecialists []SpecialistSnapshot
	WorktreeIDs       []string
	ChangedPaths      []string
	Checkpoints       []CheckpointSnapshot
}

type ExecutionSnapshot struct {
	TaskID       string
	AgentID      string
	Running      bool
	StopReason   execution.StopReason
	Verification verification.Decision
	Budget       execution.BudgetSnapshot
}

type SpecialistSnapshot struct {
	TaskID  string
	AgentID string
}

type PlanSnapshot struct {
	ID         string
	State      plan.PlanState
	StopReason plan.StopReason
	Nodes      []PlanNodeSnapshot
}

type PlanNodeSnapshot struct {
	ID    string
	State plan.NodeState
}

type CheckpointSnapshot struct {
	ID    string
	Path  string
	State string
}

type operationalExecution struct {
	snapshot ExecutionSnapshot
	runtime  *taskRuntime
}

func (o *Orchestrator) beginOperationalExecution(result ExecutionResult, mode verification.Mode, runtime *taskRuntime) {
	o.operationalMu.Lock()
	defer o.operationalMu.Unlock()
	if o.operationalActive == nil {
		o.operationalActive = make(map[string]*operationalExecution)
	}
	o.operationalActive[result.TaskID] = &operationalExecution{snapshot: ExecutionSnapshot{
		TaskID: result.TaskID, AgentID: result.AgentID, Running: true,
		Verification: verification.Decision{Allowed: mode == verification.ModeReport},
	}, runtime: runtime}
}

func (o *Orchestrator) finishOperationalExecution(result ExecutionResult, err error) {
	o.operationalMu.RLock()
	active := o.operationalActive[result.TaskID]
	o.operationalMu.RUnlock()
	snapshot := ExecutionSnapshot{
		TaskID: result.TaskID, AgentID: result.AgentID, StopReason: result.StopReason,
		Verification: cloneVerification(result.Verification), Budget: result.Budget,
	}
	if snapshot.StopReason == "" {
		snapshot.StopReason = execution.StopReasonForError(err)
	}
	if snapshot.StopReason == execution.StopSuccess && !snapshot.Verification.Allowed {
		snapshot.StopReason = execution.StopVerificationFailed
	}
	if active != nil && active.runtime != nil && snapshot.Budget.Limits == (execution.TaskLimits{}) {
		snapshot.Budget = active.runtime.budget.Snapshot()
	}
	o.operationalMu.Lock()
	delete(o.operationalActive, result.TaskID)
	o.operationalLast = snapshot
	o.operationalMu.Unlock()
}

func (o *Orchestrator) observeOperationalCompletion(initial ExecutionResult, source <-chan ExecutionCompletion) <-chan ExecutionCompletion {
	out := make(chan ExecutionCompletion, 1)
	go func() {
		defer close(out)
		completion, ok := <-source
		if !ok {
			o.finishOperationalExecution(initial, fmt.Errorf("execution completion closed without a result"))
			return
		}
		result := ApplyCompletion(initial, completion)
		o.finishOperationalExecution(result, completion.Err)
		out <- completion
	}()
	return out
}

func (o *Orchestrator) setOperationalPlan(identity PlanRuntimeIdentity) {
	copy := identity
	o.operationalMu.Lock()
	o.operationalPlan = &copy
	o.operationalMu.Unlock()
}

// OperationalState returns a bounded immutable snapshot. VCS paths come from
// git status and are intentionally distinct from the edit checkpoint journal.
func (o *Orchestrator) OperationalState(ctx context.Context) OperationalSnapshot {
	snapshot := OperationalSnapshot{CapturedAt: time.Now().UTC(), PlanOnly: o.PlanMode(), VerificationMode: o.VerificationMode()}
	if o.permissionYolo.Load() {
		snapshot.PermissionMode = string(tool.PermissionModeAutoApprove)
	} else {
		snapshot.PermissionMode = string(tool.PermissionModePrompt)
	}
	o.operationalMu.RLock()
	snapshot.Execution = cloneExecutionSnapshot(o.operationalLast)
	var activeExecutions []ExecutionSnapshot
	for _, active := range o.operationalActive {
		if active == nil {
			continue
		}
		current := cloneExecutionSnapshot(active.snapshot)
		if active.runtime != nil {
			current.Budget = active.runtime.budget.Snapshot()
		}
		if current.AgentID != "" && current.AgentID != o.primary {
			snapshot.ActiveSpecialists = append(snapshot.ActiveSpecialists, SpecialistSnapshot{TaskID: current.TaskID, AgentID: current.AgentID})
		}
		activeExecutions = append(activeExecutions, current)
	}
	var planIdentity *PlanRuntimeIdentity
	if o.operationalPlan != nil {
		copy := *o.operationalPlan
		planIdentity = &copy
	}
	o.operationalMu.RUnlock()
	sort.Slice(activeExecutions, func(i, j int) bool { return activeExecutions[i].TaskID < activeExecutions[j].TaskID })
	if len(activeExecutions) > 0 {
		snapshot.Execution = activeExecutions[0]
	}
	sort.Slice(snapshot.ActiveSpecialists, func(i, j int) bool {
		if snapshot.ActiveSpecialists[i].AgentID == snapshot.ActiveSpecialists[j].AgentID {
			return snapshot.ActiveSpecialists[i].TaskID < snapshot.ActiveSpecialists[j].TaskID
		}
		return snapshot.ActiveSpecialists[i].AgentID < snapshot.ActiveSpecialists[j].AgentID
	})
	snapshot.ActiveSpecialists = boundSpecialists(snapshot.ActiveSpecialists)

	if planIdentity != nil && o.planStore != nil {
		ref := plan.Plan{TenantID: planIdentity.TenantID, RepositoryID: planIdentity.RepositoryID, TaskID: planIdentity.TaskID, ID: planIdentity.PlanID, Generation: planIdentity.Generation}
		if current, err := o.planStore.Load(ctx, ref); err == nil {
			snapshot.Plan = clonePlanSnapshot(current)
		}
	}
	if o.worktreeManager != nil {
		if handles, err := o.worktreeManager.Recover(); err == nil {
			for _, handle := range handles {
				if handle.Manifest.CleanupState == worktree.CleanupActive || handle.Manifest.CleanupState == worktree.CleanupPrepared {
					snapshot.WorktreeIDs = append(snapshot.WorktreeIDs, handle.Manifest.ID)
				}
			}
			sort.Strings(snapshot.WorktreeIDs)
			snapshot.WorktreeIDs = boundStrings(snapshot.WorktreeIDs)
		}
	}
	snapshot.ChangedPaths = o.vcsChangedPaths(ctx)
	o.editsMu.Lock()
	for i := len(o.edits) - 1; i >= 0 && len(snapshot.Checkpoints) < maxOperationalItems; i-- {
		checkpoint := o.edits[i]
		snapshot.Checkpoints = append(snapshot.Checkpoints, CheckpointSnapshot{ID: checkpoint.ID, Path: checkpoint.Path, State: checkpoint.State})
	}
	o.editsMu.Unlock()
	return snapshot
}

func cloneExecutionSnapshot(source ExecutionSnapshot) ExecutionSnapshot {
	source.Verification = cloneVerification(source.Verification)
	return source
}

func cloneVerification(source verification.Decision) verification.Decision {
	result := source
	result.Obligations = append([]verification.Obligation(nil), source.Obligations...)
	for i := range result.Obligations {
		result.Obligations[i].Paths = append([]string(nil), source.Obligations[i].Paths...)
	}
	return result
}

func clonePlanSnapshot(source plan.Plan) PlanSnapshot {
	result := PlanSnapshot{ID: string(source.ID), State: source.State, StopReason: source.StopReason, Nodes: make([]PlanNodeSnapshot, 0, len(source.Nodes))}
	for _, node := range source.Nodes {
		result.Nodes = append(result.Nodes, PlanNodeSnapshot{ID: string(node.ID), State: node.State})
	}
	return result
}

func boundSpecialists(values []SpecialistSnapshot) []SpecialistSnapshot {
	if len(values) > maxOperationalItems {
		values = values[:maxOperationalItems]
	}
	return append([]SpecialistSnapshot(nil), values...)
}

func boundStrings(values []string) []string {
	if len(values) > maxOperationalItems {
		values = values[:maxOperationalItems]
	}
	return append([]string(nil), values...)
}

func (o *Orchestrator) vcsChangedPaths(ctx context.Context) []string {
	root := ""
	if o.workspace != nil {
		root = o.workspace.Root
	}
	if root == "" {
		return nil
	}
	output, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=all").Output()
	if err != nil {
		return nil
	}
	parts := strings.Split(string(output), "\x00")
	paths := make(map[string]struct{})
	for i := 0; i < len(parts)-1; i++ {
		record := parts[i]
		if len(record) < 4 || record[2] != ' ' {
			return nil
		}
		paths[record[3:]] = struct{}{}
		if record[0] == 'R' || record[0] == 'C' || record[1] == 'R' || record[1] == 'C' {
			i++
			if i >= len(parts)-1 || parts[i] == "" {
				return nil
			}
			paths[parts[i]] = struct{}{}
		}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return boundStrings(result)
}
