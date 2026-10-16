package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/verification"
)

func (r *taskRuntime) observationContext() context.Context {
	if r.observationCtx != nil {
		return r.observationCtx
	}
	return context.Background()
}

func (r *taskRuntime) sourceObservationsEnabled() bool {
	r.sourceMu.Lock()
	defer r.sourceMu.Unlock()
	return r.sources != nil
}

func (r *taskRuntime) backgroundMayChangeSource(args map[string]any) bool {
	command, _ := args["command"].(string)
	for _, step := range planShellEvidence(command) {
		if step.mutation || verificationClass(step.class) && step.class != execution.CommandDiff {
			return true
		}
	}
	return false
}

func shellDirectory(root string, args map[string]any) string {
	directory, _ := args["working_dir"].(string)
	if directory == "" {
		return root
	}
	if !filepath.IsAbs(directory) {
		return filepath.Join(root, directory)
	}
	return directory
}

// Observe terminal receipts without consuming the shell's output stream.
func (r *taskRuntime) reconcileBackgrounds() error {
	if r.backgroundShells == nil {
		return nil
	}
	snapshots := r.backgroundShells.Snapshots(r.observationContext())
	r.backgroundMu.Lock()
	r.backgroundSnapshots = snapshots
	r.backgroundMu.Unlock()
	for _, snapshot := range snapshots {
		if snapshot.Exited {
			// One final scan covers every exit in this lifecycle snapshot.
			if err := r.refreshSourceChanges(false); err != nil {
				return err
			}
			break
		}
	}
	for _, snapshot := range snapshots {
		if !snapshot.Exited {
			continue
		}
		args, started, revision, ok := r.finishBackground(snapshot.ID)
		if !ok {
			continue
		}
		result := map[string]any{"status": "exited", "shell_id": snapshot.ID, "exit_code": snapshot.ExitCode}
		if err := recordBackgroundChecks(r, args, result, started, snapshot.CompletedAt, revision); err != nil {
			r.trackBackground(snapshot.ID, args, started, revision)
			return fmt.Errorf("record observed background completion: %w", err)
		}
	}
	return nil
}

func (r *taskRuntime) pendingBackgroundObligations() []verification.Obligation {
	calls := map[string]map[string]any{}
	r.backgroundMu.Lock()
	for id, call := range r.background {
		calls[id] = call.args
	}
	snapshots := append([]security.ShellSnapshot(nil), r.backgroundSnapshots...)
	r.backgroundMu.Unlock()
	// Use the lifecycle snapshot whose exited processes received a final source scan.
	for _, snapshot := range snapshots {
		if !snapshot.Exited {
			calls[snapshot.ID] = map[string]any{"command": snapshot.Command, "working_dir": snapshot.Directory}
		}
	}
	var result []verification.Obligation
	for id, args := range calls {
		if r.backgroundMayChangeSource(args) {
			result = append(result, verification.Obligation{ID: "background:" + id, Kind: verification.KindQuiescence, Paths: []string{"."}, Status: verification.StatusPending})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
