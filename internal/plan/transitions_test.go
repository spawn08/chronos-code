package plan

import (
	"errors"
	"testing"
)

func TestDAGValidation(t *testing.T) {
	tests := []struct {
		name string
		plan Plan
		want error
	}{
		{"valid", Plan{Nodes: []Node{{ID: "a"}, {ID: "b"}}, Dependencies: []Dependency{{NodeID: "b", DependsOn: "a"}}}, nil},
		{"duplicate node ID", Plan{Nodes: []Node{{ID: "a"}, {ID: "a"}}}, ErrDuplicateID},
		{"dangling dependency", Plan{Nodes: []Node{{ID: "a"}}, Dependencies: []Dependency{{NodeID: "a", DependsOn: "missing"}}}, ErrInvalidDAG},
		{"cycle", Plan{Nodes: []Node{{ID: "a"}, {ID: "b"}}, Dependencies: []Dependency{{NodeID: "a", DependsOn: "b"}, {NodeID: "b", DependsOn: "a"}}}, ErrInvalidDAG},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.plan.ValidateDAG(); !errors.Is(err, test.want) {
				t.Fatalf("ValidateDAG() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestTransitionRejectsIllegalStateWithoutMutation(t *testing.T) {
	plan := Plan{State: PlanActive}
	if err := plan.Transition(PlanDraft); !errors.Is(err, ErrInvalidPlanTransition) {
		t.Fatalf("Transition() error = %v, want %v", err, ErrInvalidPlanTransition)
	}
	if plan.State != PlanActive {
		t.Fatalf("plan state = %q, want %q", plan.State, PlanActive)
	}

	node := Node{State: NodeReady}
	if err := node.Transition(NodeCompleted); !errors.Is(err, ErrInvalidNodeTransition) {
		t.Fatalf("Transition() error = %v, want %v", err, ErrInvalidNodeTransition)
	}
	if node.State != NodeReady {
		t.Fatalf("node state = %q, want %q", node.State, NodeReady)
	}
}

func TestTransitionStateMachines(t *testing.T) {
	plan := Plan{State: PlanDraft}
	if err := plan.Transition(PlanActive); err != nil {
		t.Fatal(err)
	}
	if err := plan.Transition(PlanReplanning); err != nil {
		t.Fatal(err)
	}
	if err := plan.Transition(PlanActive); err != nil {
		t.Fatal(err)
	}

	node := Node{State: NodeProposed}
	for _, state := range []NodeState{NodePending, NodeReady, NodeLeased, NodeRunning, NodeCompleted} {
		if err := node.Transition(state); err != nil {
			t.Fatalf("Transition(%q): %v", state, err)
		}
	}
}

func TestTerminalStateDerivation(t *testing.T) {
	tests := []struct {
		name  string
		nodes []Node
		state PlanState
		ok    bool
	}{
		{"in progress", []Node{{State: NodeCompleted}, {State: NodeRunning}}, "", false},
		{"completed", []Node{{State: NodeCompleted}, {State: NodeCompleted}}, PlanCompleted, true},
		{"failed wins deterministically", []Node{{State: NodeCanceled}, {State: NodeFailed}}, PlanFailed, true},
		{"canceled", []Node{{State: NodeCompleted}, {State: NodeCanceled}}, PlanCanceled, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, ok := DeriveTerminalState(test.nodes)
			if state != test.state || ok != test.ok {
				t.Fatalf("DeriveTerminalState() = (%q, %t), want (%q, %t)", state, ok, test.state, test.ok)
			}
		})
	}
}

func TestGenerationPreservesCompletedNodesAndEvidence(t *testing.T) {
	source := Plan{
		TenantID:     "tenant",
		RepositoryID: "repository",
		TaskID:       "task",
		ID:           "plan",
		Generation:   "one",
		State:        PlanActive,
		Nodes:        []Node{{ID: "done", State: NodeCompleted}, {ID: "redo", State: NodeRunning}},
		Evidence:     []Evidence{{ID: "evidence", NodeID: "done"}},
	}
	if err := source.Transition(PlanReplanning); err != nil {
		t.Fatal(err)
	}
	next, err := source.NewGeneration("two", []Node{{ID: "done", State: NodePending}, {ID: "redo", State: NodePending}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if source.Generation != "one" || source.Nodes[0].State != NodeCompleted || source.State != PlanReplanning {
		t.Fatalf("source generation was mutated: %#v", source)
	}
	if next.Generation != "two" || next.State != PlanActive || next.Nodes[0].State != NodeCompleted {
		t.Fatalf("next generation did not preserve completed node: %#v", next)
	}
	if len(next.Evidence) != 1 || next.Evidence[0].ID != "evidence" {
		t.Fatalf("next generation evidence = %#v", next.Evidence)
	}
}

func TestGenerationRejectsNonReplanningSource(t *testing.T) {
	plan := Plan{Generation: "one", State: PlanActive}
	if _, err := plan.NewGeneration("two", nil, nil); !errors.Is(err, ErrImmutableGeneration) {
		t.Fatalf("NewGeneration() error = %v, want %v", err, ErrImmutableGeneration)
	}
}
