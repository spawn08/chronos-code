package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/plan"
)

func TestPlanCommandInspectionJSON(t *testing.T) {
	database := seedPlanCommandStore(t)
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"status", []string{"status", "--db", database}, `{"schema_version":1,"healthy":true}`},
		{"list", planScopeArgs(database, "list"), `[{"task_id":"task-1","plan_id":"plan-1","generation_id":"generation-1","state":"active","stop_reason":"","version":1}]`},
		{"show", planRefArgs(database, "show"), `"idempotency_key":"[REDACTED]"`},
		{"graph", planRefArgs(database, "graph"), `"dependencies":[{"NodeID":"node-2","DependsOn":"node-1"}]`},
		{"events", planRefArgs(database, "events"), `[{"id":"event-1","node_id":"node-1","idempotency_key":"[REDACTED]"}]`},
		{"export", planScopeArgs(database, "export"), `"version":1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := runPlanForTest(t, test.args...)
			if err != nil {
				t.Fatalf("runPlan() error = %v", err)
			}
			if !strings.Contains(got, test.want) {
				t.Fatalf("runPlan() output = %s, want substring %s", got, test.want)
			}
		})
	}
}

func TestPlanCommandControlsRequireExplicitConfirmation(t *testing.T) {
	database := seedPlanCommandStore(t)
	ref := planRefArgs(database, "pause")
	if _, err := runPlanForTest(t, ref...); err == nil || !strings.Contains(err.Error(), "--yes is required") {
		t.Fatalf("pause without confirmation error = %v", err)
	}

	args := append(ref, "--expected-version", "1", "--yes")
	got, err := runPlanForTest(t, args...)
	if err != nil {
		t.Fatalf("confirmed pause error = %v", err)
	}
	if got != "{\"task_id\":\"task-1\",\"plan_id\":\"plan-1\",\"generation_id\":\"generation-1\",\"state\":\"paused\",\"stop_reason\":\"\",\"version\":2}\n" {
		t.Fatalf("confirmed pause output = %q", got)
	}

	pruneArgs := append(planScopeArgs(database, "prune"), "--dry-run")
	got, err = runPlanForTest(t, pruneArgs...)
	if err != nil {
		t.Fatalf("dry-run prune error = %v", err)
	}
	if got != "{\"plans\":1,\"rows\":9,\"dry_run\":true}\n" {
		t.Fatalf("dry-run prune output = %q", got)
	}
}

func TestPlanCommandSafeFailures(t *testing.T) {
	if _, err := runPlanForTest(t, "list"); err == nil || !strings.Contains(err.Error(), "--db is required") {
		t.Fatalf("missing database error = %v", err)
	}
	if _, err := runPlanForTest(t, "list", "--db", t.TempDir()+"/plans.db"); err == nil || !strings.Contains(err.Error(), "invalid plan scope") {
		t.Fatalf("missing scope error = %v", err)
	}
	if _, err := runPlanForTest(t, "list", "--db", "broken", "--unknown"); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("unknown flag error = %v", err)
	}

	corrupt := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runPlanForTest(t, "verify-db", "--db", corrupt); err == nil {
		t.Fatal("corrupt database error = nil")
	}
}

func seedPlanCommandStore(t *testing.T) string {
	t.Helper()
	database := filepath.Join(t.TempDir(), "plans.db")
	store, err := plan.OpenSQLStore(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	err = store.Create(context.Background(), plan.Plan{
		TenantID: "tenant-1", RepositoryID: "repository-1", TaskID: "task-1", ID: "plan-1", Generation: "generation-1", State: plan.PlanActive,
		Nodes:        []plan.Node{{ID: "node-1", State: plan.NodeReady}, {ID: "node-2", State: plan.NodePending}},
		Dependencies: []plan.Dependency{{NodeID: "node-2", DependsOn: "node-1"}},
		Attempts:     []plan.Attempt{{ID: "attempt-1", NodeID: "node-1", IdempotencyKey: "attempt-secret"}},
		ContextRefs:  []plan.ContextRef{{ID: "context-1", NodeID: "node-1"}},
		Evidence:     []plan.Evidence{{ID: "evidence-1", NodeID: "node-1"}},
		Leases:       []plan.Lease{{ID: "lease-1", AttemptID: "attempt-1"}},
		Events:       []plan.Event{{ID: "event-1", NodeID: "node-1", IdempotencyKey: "event-secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func planScopeArgs(database, operation string) []string {
	return []string{operation, "--db", database, "--tenant", "tenant-1", "--repository", "repository-1"}
}

func planRefArgs(database, operation string) []string {
	return append(planScopeArgs(database, operation), "--task", "task-1", "--plan", "plan-1", "--generation", "generation-1")
}

func runPlanForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetGlobalFlags(t, append([]string{"chronos-code", "plan"}, args...))
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = w
	err = runPlan()
	os.Stdout = originalStdout
	if closeErr := w.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	output, readErr := io.ReadAll(r)
	if closeErr := r.Close(); readErr == nil && closeErr != nil {
		readErr = closeErr
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(output), err
}
