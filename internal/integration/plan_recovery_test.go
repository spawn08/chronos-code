package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/verification"
)

func TestPlanRecovery(t *testing.T) {
	ctx := context.Background()
	fixture := loadRecoveryFixture(t)
	database := filepath.Join(t.TempDir(), "plans.db")
	effects := filepath.Join(t.TempDir(), "effects.log")
	p := recoveryPlan(fixture)

	store, err := plan.OpenSQLStore(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	scheduler := plan.NewScheduler(store, plan.SchedulerConfig{MaxAttempts: 2})
	claimed, err := scheduler.Claim(ctx, p, plan.ClaimRequest{AttemptID: "interrupted-attempt", LeaseID: "interrupted-lease", EventID: "interrupted-claim", IdempotencyKey: "effect"})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Start(ctx, p, claimed.ID, "interrupted-lease"); err != nil {
		t.Fatal(err)
	}
	recordEffect(t, effects, claimed.ID)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = plan.OpenSQLStore(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	scheduler = plan.NewScheduler(store, plan.SchedulerConfig{MaxAttempts: 2})
	if err := scheduler.Retry(ctx, p, claimed.ID, "interrupted-lease", "recovered-retry", "recovered-effect"); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Heartbeat(ctx, p, claimed.ID, "interrupted-lease"); !errors.Is(err, plan.ErrLeaseLost) {
		t.Fatalf("Heartbeat() with superseded lease error = %v, want %v", err, plan.ErrLeaseLost)
	}

	executor := &idempotentExecutor{effects: effects}
	var loadedContext []plan.ContextEntry
	controller := plan.NewController(store, executor, func(_ context.Context, _ plan.Plan, _ plan.Node) ([]plan.ContextEntry, error) {
		loadedContext = append([]plan.ContextEntry(nil), fixture.Context...)
		return loadedContext, nil
	}, nil, plan.ControllerConfig{Scheduler: plan.SchedulerConfig{MaxAttempts: 2}, ContextBytes: 1024})
	completed, err := controller.Run(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != plan.PlanCompleted {
		t.Fatalf("recovered plan state = %q, want %q", completed.State, plan.PlanCompleted)
	}
	if got := effectCount(t, effects, "effect"); got != 1 {
		t.Fatalf("committed effect executions = %d, want 1", got)
	}
	if got := effectCount(t, effects, "finish"); got != 1 {
		t.Fatalf("dependent effect executions = %d, want 1", got)
	}
	if len(loadedContext) != len(fixture.Context) || len(executor.contexts) != 2 {
		t.Fatalf("restart context = %#v, executions = %#v", loadedContext, executor.contexts)
	}

	assertReplanPreservesEvidence(t, ctx, store, fixture)
	assertFreshVerificationEvidence(t)
	assertPlanCLI(t, database, fixture)
}

type recoveryFixture struct {
	TenantID         plan.TenantID       `json:"tenant_id"`
	RepositoryID     plan.RepositoryID   `json:"repository_id"`
	TaskID           plan.TaskID         `json:"task_id"`
	PlanID           plan.PlanID         `json:"plan_id"`
	Generation       plan.GenerationID   `json:"generation"`
	Nodes            []plan.NodeID       `json:"nodes"`
	Context          []plan.ContextEntry `json:"context"`
	ReplanGeneration plan.GenerationID   `json:"replan_generation"`
}

func loadRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	path := filepath.Join("testdata", "recovery_task.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture recoveryFixture
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Nodes) != 2 || len(fixture.Context) == 0 {
		t.Fatalf("invalid recovery fixture: %#v", fixture)
	}
	return fixture
}

func recoveryPlan(fixture recoveryFixture) plan.Plan {
	return plan.Plan{
		TenantID: fixture.TenantID, RepositoryID: fixture.RepositoryID, TaskID: fixture.TaskID, ID: fixture.PlanID, Generation: fixture.Generation, State: plan.PlanActive,
		Nodes:        []plan.Node{{ID: fixture.Nodes[0], State: plan.NodePending}, {ID: fixture.Nodes[1], State: plan.NodePending}},
		Dependencies: []plan.Dependency{{NodeID: fixture.Nodes[1], DependsOn: fixture.Nodes[0]}},
		ContextRefs:  []plan.ContextRef{{ID: fixture.Context[0].ID, NodeID: fixture.Nodes[0]}},
	}
}

type idempotentExecutor struct {
	mu       sync.Mutex
	effects  string
	contexts []plan.RestartContext
}

func (e *idempotentExecutor) Execute(_ context.Context, request plan.NodeExecutionRequest) error {
	e.mu.Lock()
	e.contexts = append(e.contexts, request.Context)
	e.mu.Unlock()
	if effectCountFromPath(e.effects, request.Node.ID) == 0 {
		if err := os.WriteFile(e.effects, []byte(readEffects(e.effects)+string(request.Node.ID)+"\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func assertReplanPreservesEvidence(t *testing.T, ctx context.Context, store *plan.SQLStore, fixture recoveryFixture) {
	t.Helper()
	source := recoveryPlan(fixture)
	source.ID = "replan-source"
	source.Nodes[0].State = plan.NodeCompleted
	source.Evidence = []plan.Evidence{{ID: "completed-effect", NodeID: source.Nodes[0].ID}}
	if err := store.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	version, err := store.Version(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionPlan(ctx, source, version, plan.PlanReplanning); err != nil {
		t.Fatal(err)
	}
	source, err = store.Load(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	next, err := source.NewGeneration(fixture.ReplanGeneration, []plan.Node{{ID: fixture.Nodes[0], State: plan.NodePending}, {ID: fixture.Nodes[1], State: plan.NodePending}}, []plan.Dependency{{NodeID: fixture.Nodes[1], DependsOn: fixture.Nodes[0]}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, next); err != nil {
		t.Fatal(err)
	}
	if len(next.Evidence) != 1 || next.Evidence[0].ID != "completed-effect" || source.Generation == next.Generation {
		t.Fatalf("replan lost append-only completed evidence: source=%#v next=%#v", source, next)
	}
}

func assertPlanCLI(t *testing.T, database string, fixture recoveryFixture) {
	t.Helper()
	backup := filepath.Join(t.TempDir(), "plans-backup.db")
	restored := filepath.Join(t.TempDir(), "plans-restored.db")
	restoreBackup := filepath.Join(t.TempDir(), "before-restore.db")
	scope := []string{"--tenant", string(fixture.TenantID), "--repository", string(fixture.RepositoryID)}
	if output := runPlanCommand(t, "verify-db", "--db", database); !strings.Contains(output, `"healthy":true`) {
		t.Fatalf("verify-db output = %s", output)
	}
	if output := runPlanCommand(t, "backup", "--db", database, "--backup", backup); !strings.Contains(output, backup) {
		t.Fatalf("backup output = %s", output)
	}
	if output := runPlanCommand(t, append([]string{"export", "--db", database}, scope...)...); !strings.Contains(output, string(fixture.PlanID)) {
		t.Fatalf("export output = %s", output)
	}
	if output := runPlanCommand(t, "restore", "--db", restored, "--source", backup, "--backup", restoreBackup, "--yes"); !strings.Contains(output, restoreBackup) {
		t.Fatalf("restore output = %s", output)
	}
	if output := runPlanCommand(t, "verify-db", "--db", restored); !strings.Contains(output, `"healthy":true`) {
		t.Fatalf("restored verify-db output = %s", output)
	}
	if output := runPlanCommand(t, append([]string{"export", "--db", restored}, scope...)...); !strings.Contains(output, string(fixture.PlanID)) {
		t.Fatalf("restored export output = %s", output)
	}
}

func assertFreshVerificationEvidence(t *testing.T) {
	t.Helper()
	const path = "internal/example/bug.go"
	obligations := verification.Derive(verification.Input{
		TaskKind: "debug", ChangedPaths: []string{path}, TestCommands: []string{"go test ./internal/example"},
	})
	events := []execution.Event{
		{ID: "write", TaskID: "recovery-task", Sequence: 1, Type: execution.EventWrite, Paths: []string{path}},
		{ID: "verify", TaskID: "recovery-task", Sequence: 2, Type: execution.EventVerification, EvidenceID: "verify", Paths: []string{path}, Detail: "go test ./internal/example", Passed: true},
		{ID: "later-write", TaskID: "recovery-task", Sequence: 3, Type: execution.EventWrite, Paths: []string{path}},
	}
	if decision := verification.Assess(verification.ModeEnforce, true, obligations, events); decision.Allowed || !decision.Disagreement {
		t.Fatalf("stale verification completion = %#v, want rejected disagreement", decision)
	}

	events = append(events, execution.Event{ID: "fresh-verify", TaskID: "recovery-task", Sequence: 4, Type: execution.EventVerification, EvidenceID: "fresh-verify", Paths: []string{path}, Detail: "go test ./internal/example", Passed: true})
	if decision := verification.Assess(verification.ModeEnforce, true, obligations, events); !decision.Allowed || decision.Disagreement {
		t.Fatalf("fresh verification completion = %#v, want allowed completion", decision)
	}
}

func runPlanCommand(t *testing.T, args ...string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate integration test")
	}
	command := exec.Command("go", append([]string{"run", "./cmd/chronos-code", "plan"}, args...)...)
	command.Dir = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("chronos-code plan %s: %v\n%s", args[0], err, output)
	}
	return string(output)
}

func recordEffect(t *testing.T, path string, node plan.NodeID) {
	t.Helper()
	if err := os.WriteFile(path, []byte(string(node)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func effectCount(t *testing.T, path string, node plan.NodeID) int {
	t.Helper()
	return effectCountFromPath(path, node)
}

func effectCountFromPath(path string, node plan.NodeID) int {
	count := 0
	for _, effect := range strings.Fields(readEffects(path)) {
		if effect == string(node) {
			count++
		}
	}
	return count
}

func readEffects(path string) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(contents)
}
