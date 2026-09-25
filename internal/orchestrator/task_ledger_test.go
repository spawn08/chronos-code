package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
)

func ledgerOrch(t *testing.T, persist bool) (*Orchestrator, string) {
	t.Helper()
	t.Setenv("CHRONOS_CODE_DATA_HOME", t.TempDir())
	cfg := &config.Config{}
	cfg.Workspace.Root = t.TempDir()
	cfg.Ledger.Persist = persist
	return &Orchestrator{cfg: cfg}, cfg.Workspace.Root
}

// writeAndVerify records a real write of body to main.go followed by a
// passing test command, returning the verification evidence ID.
func writeAndVerify(t *testing.T, runtime *taskRuntime, root, body string) execution.EvidenceID {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	state := readFileState(root, "main.go")
	if _, err := runtime.recordWrite("main.go", state.hash, state.size, execution.ProvenanceRuntime, time.Now()); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	event, err := runtime.recordCommand("go test ./...", execution.CommandTest, &exitCode, execution.TerminalExited, nil, execution.ProvenanceRuntime, time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return event.EvidenceID
}

func currentEvidence(t *testing.T, runtime *taskRuntime, id execution.EvidenceID) (execution.Verification, bool) {
	t.Helper()
	state, err := runtime.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	evidence, ok := state.Verification[id]
	return evidence, ok
}

func TestPersistedLedgerCarriesEvidenceAcrossExecutions(t *testing.T) {
	o, root := ledgerOrch(t, true)
	first, err := o.openTaskRuntime("plan-node-1", true, root)
	if err != nil {
		t.Fatal(err)
	}
	evidenceID := writeAndVerify(t, first, root, "package main\n")

	second, err := o.openTaskRuntime("plan-node-1", true, root)
	if err != nil {
		t.Fatal(err)
	}
	evidence, ok := currentEvidence(t, second, evidenceID)
	if !ok || !evidence.Current {
		t.Fatalf("evidence from earlier execution = %#v ok=%v, want current", evidence, ok)
	}
	var result ExecutionResult
	populateRuntimeResult(&result, second)
	if len(result.ChangedPaths) != 1 || result.ChangedPaths[0] != "main.go" || len(result.EvidenceIDs) != 1 {
		t.Fatalf("resumed result = %#v", result)
	}
}

func TestPersistedLedgerInvalidatesEvidenceOnExternalChange(t *testing.T) {
	o, root := ledgerOrch(t, true)
	first, err := o.openTaskRuntime("plan-node-1", true, root)
	if err != nil {
		t.Fatal(err)
	}
	evidenceID := writeAndVerify(t, first, root, "package main\n")

	// Edited outside the agent between executions.
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := o.openTaskRuntime("plan-node-1", true, root)
	if err != nil {
		t.Fatal(err)
	}
	if evidence, _ := currentEvidence(t, second, evidenceID); evidence.Current {
		t.Fatal("evidence must be stale after an out-of-band edit to a verified file")
	}
	state, err := second.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	last := state.Writes[len(state.Writes)-1]
	if last.Detail != externalChangeDetail || last.ContentHash != readFileState(root, "main.go").hash {
		t.Fatalf("reconciliation write = %#v", last)
	}

	// Reconciliation is idempotent: an unchanged workspace adds no writes.
	third, err := o.openTaskRuntime("plan-node-1", true, root)
	if err != nil {
		t.Fatal(err)
	}
	thirdState, err := third.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(thirdState.Writes) != len(state.Writes) {
		t.Fatalf("writes after idempotent reopen = %d, want %d", len(thirdState.Writes), len(state.Writes))
	}
}

func TestLedgerNotPersistedWithoutOptInOrStableTaskID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		persist  bool
		explicit bool
	}{
		{name: "disabled", persist: false, explicit: true},
		{name: "generated task ID", persist: true, explicit: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, root := ledgerOrch(t, tc.persist)
			first, err := o.openTaskRuntime("task", tc.explicit, root)
			if err != nil {
				t.Fatal(err)
			}
			writeAndVerify(t, first, root, "package main\n")
			second, err := o.openTaskRuntime("task", tc.explicit, root)
			if err != nil {
				t.Fatal(err)
			}
			if events := second.ledger.Events(); len(events) != 0 {
				t.Fatalf("unexpected persisted events: %d", len(events))
			}
		})
	}
}
