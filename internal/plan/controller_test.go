package plan

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestControllerCompletesPlanAndResumesWithoutReplayingNode(t *testing.T) {
	ctx, _, p := schedulerPlan(t, []Node{{ID: "a", State: NodePending}, {ID: "b", State: NodePending}}, []Dependency{{NodeID: "b", DependsOn: "a"}}, SchedulerConfig{})
	store := openTestSQLStore(t)
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{}
	controller := NewController(store, executor, func(context.Context, Plan, Node) ([]ContextEntry, error) {
		return []ContextEntry{{ID: "context", Content: "value"}}, nil
	}, verifierFunc(func(context.Context, Plan, Node) error { return nil }), ControllerConfig{ContextBytes: 100})

	got, err := controller.Run(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != PlanCompleted || executor.count("a") != 1 || executor.count("b") != 1 {
		t.Fatalf("run = %#v, executions = %#v", got, executor.calls)
	}
	got, err = controller.Run(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != PlanCompleted || executor.count("a") != 1 || executor.count("b") != 1 {
		t.Fatalf("resume replayed committed work: %#v", executor.calls)
	}
}

func TestControllerSerializesOverlappingWritesAndRunsReadOnlyNodesTogether(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLStore(t)
	p := Plan{TenantID: "tenant", RepositoryID: "repo", TaskID: "task", ID: "plan", Generation: "one", State: PlanActive, Nodes: []Node{{ID: "read-a", State: NodePending}, {ID: "read-b", State: NodePending}, {ID: "write-a", State: NodePending}, {ID: "write-b", State: NodePending}}}
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{access: map[NodeID]NodeAccess{"read-a": {ReadOnly: true}, "read-b": {ReadOnly: true}, "write-a": {Paths: []string{"same"}}, "write-b": {Paths: []string{"same"}}}, releaseReads: make(chan struct{})}
	controller := NewController(store, executor, nil, nil, ControllerConfig{})
	if _, err := controller.Run(ctx, p); err != nil {
		t.Fatal(err)
	}
	if executor.maxRead < 2 || executor.maxWrite > 1 {
		t.Fatalf("max concurrent reads = %d, writes = %d", executor.maxRead, executor.maxWrite)
	}
}

func TestControllerPersistsVerificationAndBudgetStops(t *testing.T) {
	for _, test := range []struct {
		name    string
		execErr error
		verify  error
		state   PlanState
		reason  StopReason
	}{
		{"budget", &StopError{Reason: StopBudgetExhausted}, nil, PlanPaused, StopBudgetExhausted},
		{"verification", nil, errors.New("failed"), PlanFailed, StopVerificationFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestSQLStore(t)
			p := Plan{TenantID: "tenant", RepositoryID: "repo", TaskID: "task", ID: "plan", Generation: "one", State: PlanActive, Nodes: []Node{{ID: "node", State: NodePending}}}
			if err := store.Create(ctx, p); err != nil {
				t.Fatal(err)
			}
			controller := NewController(store, executorFunc(func(context.Context, NodeExecutionRequest) error { return test.execErr }), nil, verifierFunc(func(context.Context, Plan, Node) error { return test.verify }), ControllerConfig{})
			got, err := controller.Run(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != test.state || got.StopReason != test.reason {
				t.Fatalf("stop = %#v, want %q / %q", got, test.state, test.reason)
			}
		})
	}
}

type executorFunc func(context.Context, NodeExecutionRequest) error

func (f executorFunc) Execute(ctx context.Context, request NodeExecutionRequest) error {
	return f(ctx, request)
}

type verifierFunc func(context.Context, Plan, Node) error

func (f verifierFunc) Verify(ctx context.Context, p Plan, node Node) error { return f(ctx, p, node) }

type recordingExecutor struct {
	mu           sync.Mutex
	calls        map[NodeID]int
	access       map[NodeID]NodeAccess
	reads        int
	writes       int
	maxRead      int
	maxWrite     int
	releaseReads chan struct{}
	releaseOnce  sync.Once
}

func (e *recordingExecutor) Execute(_ context.Context, request NodeExecutionRequest) error {
	e.mu.Lock()
	if e.calls == nil {
		e.calls = make(map[NodeID]int)
	}
	e.calls[request.Node.ID]++
	access := e.access[request.Node.ID]
	if access.ReadOnly {
		e.reads++
		if e.reads > e.maxRead {
			e.maxRead = e.reads
		}
		if e.releaseReads != nil && e.reads == 2 {
			e.releaseOnce.Do(func() { close(e.releaseReads) })
		}
	} else {
		e.writes++
		if e.writes > e.maxWrite {
			e.maxWrite = e.writes
		}
	}
	e.mu.Unlock()
	if access.ReadOnly {
		if e.releaseReads != nil {
			<-e.releaseReads
		}
	}
	e.mu.Lock()
	if access.ReadOnly {
		e.reads--
	} else {
		e.writes--
	}
	e.mu.Unlock()
	return nil
}

func (e *recordingExecutor) Access(_ context.Context, _ Plan, node Node) (NodeAccess, error) {
	return e.access[node.ID], nil
}

func (e *recordingExecutor) count(id NodeID) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[id]
}
