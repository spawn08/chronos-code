package orchestrator

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/verification"
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
	ActiveSpecialists []SpecialistSnapshot
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
	if active != nil && active.runtime != nil && active.runtime.observationCtx != nil && o.sessionMgr != nil && o.store != nil {
		ctx, cancel := context.WithTimeout(active.runtime.observationCtx, 5*time.Second)
		defer cancel()
		status := "failed"
		if snapshot.StopReason == execution.StopSuccess {
			status = "completed"
			if snapshot.Verification.Disagreement {
				status = "partial"
			}
		}
		if snapshot.StopReason == execution.StopNoProgress || snapshot.StopReason == execution.StopMaxTurns {
			status = "paused"
		}
		_ = o.sessionMgr.Touch(ctx, result.SessionID, status)
		if events, err := o.store.ListEvents(ctx, result.SessionID, 0); err == nil {
			seq := int64(1)
			for _, event := range events {
				if event.SeqNum >= seq {
					seq = event.SeqNum + 1
				}
			}
			_ = o.store.AppendEvent(ctx, &storage.Event{ID: "terminal-" + session.NewSessionID(), SessionID: result.SessionID, SeqNum: seq, Type: "harness_execution_finished", Payload: map[string]any{"task_id": result.TaskID, "status": status, "stop_reason": snapshot.StopReason, "cost_microdollars": result.CostMicrodollars}, CreatedAt: time.Now().UTC()})
		}
	}
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

// OperationalState returns a bounded immutable snapshot. VCS paths come from
// git status and are intentionally distinct from the edit checkpoint journal.
func (o *Orchestrator) OperationalState(ctx context.Context) OperationalSnapshot {
	snapshot := OperationalSnapshot{CapturedAt: time.Now().UTC(), PlanOnly: o.PlanMode(), VerificationMode: o.VerificationMode()}
	if o.permissionSkip.Load() {
		snapshot.PermissionMode = PermissionModeSkip
	} else if o.permissionYolo.Load() {
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

// recoveryDigest uses observed task versions and ledger evidence, not VCS
// status or selected repository context, to describe resumable working state.
func (r *taskRuntime) recoveryDigest() string {
	if r == nil {
		return ""
	}
	lines := []string{fmt.Sprintf("Active task: %s (source=task-runtime)", r.taskID)}
	r.sourceMu.Lock()
	paths := make([]string, 0, len(r.sources))
	for path := range r.sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths[:min(len(paths), maxOperationalItems)] {
		lines = append(lines, fmt.Sprintf("Observed file version: %s sha256=%s", path, r.sources[path]))
	}
	r.sourceMu.Unlock()
	if len(paths) > maxOperationalItems {
		lines = append(lines, fmt.Sprintf("Partial observed versions: %d more inputs omitted; re-read omitted sources before accepting checks.", len(paths)-maxOperationalItems))
	}
	if r.ledger == nil {
		lines = append(lines, "Task ledger unavailable; checks are unverified.")
		lines = append(lines, r.claimsDigest()...)
		return strings.Join(lines, "\n")
	}
	if state, err := r.ledger.State(); err == nil {
		ids := make([]string, 0, len(state.Verification))
		for id := range state.Verification {
			ids = append(ids, string(id))
		}
		sort.Slice(ids, func(i, j int) bool {
			left, right := state.Verification[execution.EvidenceID(ids[i])], state.Verification[execution.EvidenceID(ids[j])]
			if left.Event.Sequence == right.Event.Sequence {
				return ids[i] < ids[j]
			}
			return left.Event.Sequence < right.Event.Sequence
		})
		changed := map[string]string{}
		for _, event := range state.Writes {
			for _, path := range event.Paths {
				changed[path] = event.ContentHash
			}
		}
		changedPaths := make([]string, 0, len(changed))
		for path := range changed {
			changedPaths = append(changedPaths, path)
		}
		sort.Strings(changedPaths)
		for _, path := range changedPaths[:min(len(changedPaths), maxOperationalItems)] {
			hash := changed[path]
			if hash == "" {
				hash = "unknown; re-read source"
			}
			lines = append(lines, fmt.Sprintf("Changed-file version: %s sha256=%s", path, hash))
		}
		if len(changedPaths) > maxOperationalItems {
			lines = append(lines, fmt.Sprintf("Partial changes: %d more changed-file versions in task ledger.", len(changedPaths)-maxOperationalItems))
		}
		for _, id := range ids[max(0, len(ids)-maxOperationalItems):] {
			check := state.Verification[execution.EvidenceID(id)]
			lines = append(lines, fmt.Sprintf("Check: id=%s accepted=%t revision=%d command=%s", id, check.Current, check.Event.MutationRevision, check.Event.Command))
		}
		if len(ids) > maxOperationalItems {
			lines = append(lines, fmt.Sprintf("Partial checks: %d more in task ledger.", len(ids)-maxOperationalItems))
		}
		for _, event := range state.Uncertainties[max(0, len(state.Uncertainties)-maxOperationalItems):] {
			lines = append(lines, "Unresolved effect: "+event.Detail)
		}
	} else {
		lines = append(lines, "Task ledger unavailable; checks are unverified.")
	}
	lines = append(lines, r.claimsDigest()...)
	return strings.Join(lines, "\n")
}
