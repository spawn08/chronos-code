package orchestrator

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
)

// externalChangeDetail marks a runtime-synthesized write for a file whose
// on-disk content no longer matches the last write the ledger recorded.
const externalChangeDetail = "external change detected when reopening persisted task ledger"

// openTaskRuntime builds the runtime for one execution. With ledger.persist
// enabled and a caller-supplied (stable) task ID, the evidence ledger is
// replayed from durable storage so verification evidence survives across
// turns, crashes, and resumes. Generated task IDs are single-use and stay
// in memory.
func (o *Orchestrator) openTaskRuntime(taskID string, explicitTaskID bool, workspaceRoot string) (*taskRuntime, error) {
	runtime, err := newTaskRuntimeWithLimits(taskID, workspaceRoot, taskLimits(o.cfg))
	if err != nil {
		return nil, err
	}
	if !explicitTaskID || o.cfg == nil || !o.cfg.Ledger.Persist {
		return runtime, nil
	}
	paths, err := o.cfg.ResolveProjectPaths("")
	if err != nil {
		return nil, fmt.Errorf("resolve evidence ledger storage: %w", err)
	}
	store := execution.NewFileLedgerStore(filepath.Join(paths.Dir, "ledgers"))
	ledger, err := store.Open(runtime.taskID)
	if err != nil {
		return nil, fmt.Errorf("open evidence ledger for task %q: %w", taskID, err)
	}
	runtime.ledger = ledger
	if err := runtime.reconcileExternalChanges(time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("reconcile evidence ledger for task %q: %w", taskID, err)
	}
	return runtime, nil
}

// reconcileExternalChanges compares each file's last recorded content hash
// with the workspace. Divergent files get a synthetic write so evidence that
// predates the out-of-band edit is no longer current. Workspace-scope
// mutations carry no per-file hash and are left as recorded.
func (r *taskRuntime) reconcileExternalChanges(now time.Time) error {
	state, err := r.ledger.State()
	if err != nil {
		return err
	}
	lastHash := make(map[string]string)
	var changed []string
	var order []string
	for _, write := range state.Writes {
		for _, scope := range write.Scopes {
			if scope.Kind != execution.ScopeExact {
				continue
			}
			if _, seen := lastHash[scope.Path]; !seen {
				order = append(order, scope.Path)
			}
			lastHash[scope.Path] = write.ContentHash
		}
	}
	for _, path := range order {
		current := readFileState(r.workspaceRoot, path)
		if current.hash == lastHash[path] {
			continue
		}
		if _, err := r.ledger.Record(execution.Event{
			Type:        execution.EventWrite,
			Scopes:      []execution.Scope{{Kind: execution.ScopeExact, Path: path}},
			Paths:       []string{path},
			ContentHash: current.hash,
			SizeBytes:   current.size,
			Detail:      externalChangeDetail,
			Provenance:  execution.ProvenanceRuntime,
			CompletedAt: now,
		}); err != nil {
			return err
		}
		changed = append(changed, path)
	}
	if len(changed) == 0 {
		return nil
	}
	return r.refreshClaims(now, changed...)
}
