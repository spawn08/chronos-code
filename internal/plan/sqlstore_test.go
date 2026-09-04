package plan

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSQLRoundTrip(t *testing.T) {
	store := openTestSQLStore(t)
	p := testPlan()
	if err := store.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 2 || len(got.Dependencies) != 1 || len(got.Attempts) != 1 || len(got.ContextRefs) != 1 || len(got.Evidence) != 1 || len(got.Leases) != 1 || len(got.Events) != 1 {
		t.Fatalf("loaded plan lost identities: %#v", got)
	}
	if got.Nodes[0].ID != "a" || got.Dependencies[0].NodeID != "b" || got.Events[0].ID != "event" {
		t.Fatalf("loaded identities = %#v", got)
	}
}

func TestSQLStaleTransitionHasOneWinner(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	p := testPlan()
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	version, err := store.Version(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- store.TransitionPlan(ctx, p, version, PlanActive) }()
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
			continue
		}
		if !errors.Is(err, ErrStaleVersion) {
			t.Fatalf("transition error = %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want 1", winners)
	}
}

func TestSQLMigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "plans.db")
	store := openTestSQLStorePath(t, path)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestSQLStorePath(t, path)
	defer store.Close()
	var versions int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM plan_schema_migrations`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("migration rows = %d, want 1", versions)
	}
}

func TestSQLRefusesNewerSchemaWithoutMutation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "plans.db")
	store := openTestSQLStorePath(t, path)
	if _, err := store.db.Exec(`INSERT INTO plan_schema_migrations (version, checksum) VALUES (2, 'future')`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSQLStore(ctx, path); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("OpenSQLStore() error = %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var versions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM plan_schema_migrations`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Fatalf("migration rows = %d, want 2", versions)
	}
}

func TestSQLOperationsScopeAndRedaction(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	inside := testPlan()
	outside := testPlan()
	outside.TenantID = "other-tenant"
	outside.RepositoryID = "other-repo"
	outside.ID = "other-plan"
	if err := store.Create(ctx, inside); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, outside); err != nil {
		t.Fatal(err)
	}
	scope := PlanScope{TenantID: inside.TenantID, RepositoryID: inside.RepositoryID}
	plans, err := store.List(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].PlanID != inside.ID {
		t.Fatalf("scoped plans = %#v", plans)
	}
	ref := PlanRef{TaskID: inside.TaskID, PlanID: inside.ID, Generation: inside.Generation}
	events, err := store.Events(ctx, scope, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].IdempotencyKey != redactedIdempotencyKey {
		t.Fatalf("events = %#v", events)
	}
	view, err := store.Show(ctx, scope, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Attempts) != 1 || view.Attempts[0].ID != "attempt" || view.Events[0].IdempotencyKey != redactedIdempotencyKey {
		t.Fatalf("redacted view = %#v", view)
	}
	graph, err := store.Graph(ctx, scope, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) != 2 || len(graph.Dependencies) != 1 {
		t.Fatalf("graph = %#v", graph)
	}
	exported, err := store.Export(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.Plans) != 1 || exported.Plans[0].Events[0].IdempotencyKey != redactedIdempotencyKey {
		t.Fatalf("export = %#v", exported)
	}
	if _, err := store.List(ctx, PlanScope{}); !errors.Is(err, ErrInvalidPlanScope) {
		t.Fatalf("List without scope error = %v", err)
	}
}

func TestSQLOperationsControlPreservesVersionAndSchedulerSemantics(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	scope := PlanScope{TenantID: "tenant", RepositoryID: "repo"}
	controlled := Plan{TenantID: scope.TenantID, RepositoryID: scope.RepositoryID, TaskID: "task", ID: "controlled", Generation: "one", State: PlanActive, Nodes: []Node{{ID: "node", State: NodeReady}}}
	if err := store.Create(ctx, controlled); err != nil {
		t.Fatal(err)
	}
	ref := PlanRef{TaskID: controlled.TaskID, PlanID: controlled.ID, Generation: controlled.Generation}
	if _, err := store.Pause(ctx, PlanControlRequest{Scope: scope, Ref: ref, ExpectedVersion: 1}); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.Resume(ctx, PlanControlRequest{Scope: scope, Ref: ref, ExpectedVersion: 2})
	if err != nil || resumed.State != PlanActive || resumed.Version != 3 {
		t.Fatalf("resume = %#v, %v", resumed, err)
	}
	if _, err := store.Cancel(ctx, PlanControlRequest{Scope: scope, Ref: ref, ExpectedVersion: 2}); !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("stale cancel error = %v", err)
	}
	canceled, err := store.Cancel(ctx, PlanControlRequest{Scope: scope, Ref: ref, ExpectedVersion: 3})
	if err != nil || canceled.State != PlanCanceled || canceled.Version != 4 {
		t.Fatalf("cancel = %#v, %v", canceled, err)
	}
	if _, err := store.Resume(ctx, PlanControlRequest{Ref: ref, ExpectedVersion: 4}); !errors.Is(err, ErrInvalidPlanScope) {
		t.Fatalf("resume without scope error = %v", err)
	}

	retryPlan := Plan{TenantID: scope.TenantID, RepositoryID: scope.RepositoryID, TaskID: "task", ID: "retry", Generation: "one", State: PlanActive, Nodes: []Node{{ID: "node", State: NodeReady}}}
	if err := store.Create(ctx, retryPlan); err != nil {
		t.Fatal(err)
	}
	claimed, err := NewScheduler(store, SchedulerConfig{MaxAttempts: 2}).Claim(ctx, retryPlan, ClaimRequest{AttemptID: "attempt", LeaseID: "lease", EventID: "claim", IdempotencyKey: "claim-key"})
	if err != nil || claimed.ID != "node" || claimed.State != NodeLeased {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	retryRef := PlanRef{TaskID: retryPlan.TaskID, PlanID: retryPlan.ID, Generation: retryPlan.Generation}
	if err := store.Retry(ctx, RetryRequest{Scope: scope, Ref: retryRef, NodeID: "node", LeaseID: "lease", EventID: "retry", IdempotencyKey: "retry-key", MaxAttempts: 2}); err != nil {
		t.Fatal(err)
	}
	retried, err := store.Show(ctx, scope, retryRef)
	if err != nil || retried.Nodes[0].State != NodeRetryWait || len(retried.Leases) != 0 || len(retried.Events) != 2 {
		t.Fatalf("retry result = %#v, %v", retried, err)
	}
	if err := store.Retry(ctx, RetryRequest{Ref: retryRef, NodeID: "node", LeaseID: "lease", EventID: "retry-again", IdempotencyKey: "retry-key"}); !errors.Is(err, ErrInvalidPlanScope) {
		t.Fatalf("retry without scope error = %v", err)
	}
}

func TestSQLOperationsPruneRequiresScopeAndPreservesOthers(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	inside := testPlan()
	outside := testPlan()
	outside.TenantID = "other-tenant"
	outside.RepositoryID = "other-repo"
	outside.ID = "other-plan"
	if err := store.Create(ctx, inside); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, outside); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(ctx, PruneRequest{}); !errors.Is(err, ErrInvalidPlanScope) {
		t.Fatalf("Prune without scope error = %v", err)
	}
	scope := PlanScope{TenantID: inside.TenantID, RepositoryID: inside.RepositoryID}
	dryRun, err := store.Prune(ctx, PruneRequest{Scope: scope, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dryRun.Plans != 1 || dryRun.Rows != 9 || !dryRun.DryRun {
		t.Fatalf("dry run = %#v", dryRun)
	}
	if _, err := store.Show(ctx, scope, PlanRef{TaskID: inside.TaskID, PlanID: inside.ID, Generation: inside.Generation}); err != nil {
		t.Fatalf("dry run mutated store: %v", err)
	}
	pruned, err := store.Prune(ctx, PruneRequest{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if pruned.Plans != 1 || pruned.Rows != 9 {
		t.Fatalf("pruned = %#v", pruned)
	}
	plans, err := store.List(ctx, PlanScope{TenantID: outside.TenantID, RepositoryID: outside.RepositoryID})
	if err != nil || len(plans) != 1 || plans[0].PlanID != outside.ID {
		t.Fatalf("outside plans = %#v, %v", plans, err)
	}
}

func TestSQLOperationsIntegrityBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	targetPath := filepath.Join(t.TempDir(), "target.db")
	store := openTestSQLStorePath(t, targetPath)
	target := testPlan()
	if err := store.Create(ctx, target); err != nil {
		t.Fatal(err)
	}
	integrity, err := store.Integrity(ctx)
	if err != nil || !integrity.Healthy || integrity.SchemaVersion != schemaVersion {
		t.Fatalf("integrity = %#v, %v", integrity, err)
	}

	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if _, err := store.Backup(ctx, BackupRequest{Path: backupPath}); err != nil {
		t.Fatal(err)
	}
	backupStore := openTestSQLStorePath(t, backupPath)
	backupPlans, err := backupStore.List(ctx, PlanScope{TenantID: target.TenantID, RepositoryID: target.RepositoryID})
	if err != nil || len(backupPlans) != 1 {
		t.Fatalf("backup plans = %#v, %v", backupPlans, err)
	}

	sourcePath := filepath.Join(t.TempDir(), "source.db")
	sourceStore := openTestSQLStorePath(t, sourcePath)
	source := testPlan()
	source.ID = "restored"
	if err := sourceStore.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := sourceStore.Close(); err != nil {
		t.Fatal(err)
	}
	restoreBackupPath := filepath.Join(t.TempDir(), "before-restore.db")
	if _, err := store.Restore(ctx, RestoreRequest{SourcePath: sourcePath, BackupPath: restoreBackupPath}); err != nil {
		t.Fatal(err)
	}
	restored, err := store.List(ctx, PlanScope{TenantID: source.TenantID, RepositoryID: source.RepositoryID})
	if err != nil || len(restored) != 1 || restored[0].PlanID != source.ID {
		t.Fatalf("restored plans = %#v, %v", restored, err)
	}
	previousStore := openTestSQLStorePath(t, restoreBackupPath)
	previous, err := previousStore.List(ctx, PlanScope{TenantID: target.TenantID, RepositoryID: target.RepositoryID})
	if err != nil || len(previous) != 1 || previous[0].PlanID != target.ID {
		t.Fatalf("pre-restore backup plans = %#v, %v", previous, err)
	}
}

func TestSQLOperationsRestoreRejectsInvalidSourceWithoutMutation(t *testing.T) {
	ctx := context.Background()
	targetPath := filepath.Join(t.TempDir(), "target.db")
	store := openTestSQLStorePath(t, targetPath)
	target := testPlan()
	if err := store.Create(ctx, target); err != nil {
		t.Fatal(err)
	}
	invalidPath := filepath.Join(t.TempDir(), "invalid.db")
	if err := os.WriteFile(invalidPath, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if _, err := store.Restore(ctx, RestoreRequest{SourcePath: invalidPath, BackupPath: backupPath}); err == nil {
		t.Fatal("Restore accepted corrupt source")
	}
	if _, err := os.Stat(backupPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid restore created backup: %v", err)
	}
	plans, err := store.List(ctx, PlanScope{TenantID: target.TenantID, RepositoryID: target.RepositoryID})
	if err != nil || len(plans) != 1 || plans[0].PlanID != target.ID {
		t.Fatalf("invalid restore mutated target: %#v, %v", plans, err)
	}
}

func openTestSQLStore(t *testing.T) *SQLStore {
	return openTestSQLStorePath(t, filepath.Join(t.TempDir(), "plans.db"))
}
func openTestSQLStorePath(t *testing.T, path string) *SQLStore {
	t.Helper()
	store, err := OpenSQLStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func testPlan() Plan {
	return Plan{TenantID: "tenant", RepositoryID: "repo", TaskID: "task", ID: "plan", Generation: "one", State: PlanDraft, Nodes: []Node{{ID: "a", State: NodePending}, {ID: "b", State: NodeProposed}}, Dependencies: []Dependency{{NodeID: "b", DependsOn: "a"}}, Attempts: []Attempt{{ID: "attempt", NodeID: "a", IdempotencyKey: "attempt-key"}}, ContextRefs: []ContextRef{{ID: "context", NodeID: "a"}}, Evidence: []Evidence{{ID: "evidence", NodeID: "a"}}, Leases: []Lease{{ID: "lease", AttemptID: "attempt"}}, Events: []Event{{ID: "event", NodeID: "a", IdempotencyKey: "event-key"}}}
}
