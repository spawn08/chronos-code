package plan

import (
	"context"
	"testing"
)

func TestPruneLimitBoundsTransactionalBatch(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	first := testPlan()
	second := testPlan()
	second.TaskID = "task-two"
	second.ID = "plan-two"
	if err := store.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	scope := PlanScope{TenantID: first.TenantID, RepositoryID: first.RepositoryID}
	dry, err := store.Prune(ctx, PruneRequest{Scope: scope, DryRun: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	real, err := store.Prune(ctx, PruneRequest{Scope: scope, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Plans != 1 || dry.Rows != real.Rows || real.Plans != 1 {
		t.Fatalf("dry=%+v real=%+v", dry, real)
	}
	remaining, err := store.List(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("remaining plans = %d, want 1", len(remaining))
	}
}
