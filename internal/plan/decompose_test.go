package plan

import (
	"context"
	"errors"
	"testing"
)

func TestDecomposePersistsValidatedDraft(t *testing.T) {
	store := openTestSQLStore(t)
	request := validDecompositionRequest()

	got, err := Decompose(context.Background(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != PlanDraft {
		t.Fatalf("plan state = %q, want %q", got.State, PlanDraft)
	}
	if len(got.Nodes) != 2 || got.Nodes[0].State != NodePending || len(got.Dependencies) != 1 {
		t.Fatalf("decomposition = %#v, want pending DAG", got)
	}
	loaded, err := store.Load(context.Background(), got)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != PlanDraft || len(loaded.ContextRefs) != 4 {
		t.Fatalf("persisted plan = %#v, want draft with source, classifier, and node references", loaded)
	}
}

func TestDecomposeRejectsInvalidGraphBeforePersistence(t *testing.T) {
	store := openTestSQLStore(t)
	request := validDecompositionRequest()
	request.Nodes[0].DependsOn = []NodeID{"verify"}

	_, err := Decompose(context.Background(), store, request)
	if !errors.Is(err, ErrInvalidDecomposition) || !errors.Is(err, ErrInvalidDAG) {
		t.Fatalf("Decompose() error = %v, want invalid decomposition DAG", err)
	}
	plans, err := store.List(context.Background(), PlanScope{TenantID: request.TenantID, RepositoryID: request.RepositoryID})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 {
		t.Fatalf("persisted plans = %#v, want none", plans)
	}
}

func TestDecomposeRequiresExecutableNodeDetails(t *testing.T) {
	store := openTestSQLStore(t)
	request := validDecompositionRequest()
	request.Nodes[0].Verification = " "

	_, err := Decompose(context.Background(), store, request)
	if !errors.Is(err, ErrInvalidDecomposition) {
		t.Fatalf("Decompose() error = %v, want invalid decomposition", err)
	}
}

func validDecompositionRequest() DecompositionRequest {
	return DecompositionRequest{
		TenantID:         "tenant",
		RepositoryID:     "repository",
		TaskID:           "task",
		PlanID:           "plan",
		Generation:       "one",
		SourceRequestRef: "request-1",
		ClassifierRef:    "classifier-1",
		Nodes: []DecompositionNode{
			{ID: "implement", Scope: "internal/plan/decompose.go", ContextRefs: []ContextID{"graph-plan"}, Risks: []string{"invalid DAG"}, Verification: "go test ./internal/plan -run TestDecompose"},
			{ID: "verify", DependsOn: []NodeID{"implement"}, Scope: "internal/plan/decompose_test.go", ContextRefs: []ContextID{"test-plan"}, Risks: []string{"missing regression"}, Verification: "go test ./internal/plan -run TestDecompose"},
		},
	}
}
