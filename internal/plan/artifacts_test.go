package plan

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestAcceptedArtifactIDsUsesOnlyCompletedTransitivePredecessors(t *testing.T) {
	p := Plan{
		Nodes:        []Node{{ID: "a", State: NodeCompleted}, {ID: "b", State: NodeCompleted}, {ID: "c", State: NodeCompleted}, {ID: "target", State: NodeReady}, {ID: "unrelated", State: NodeCompleted}},
		Dependencies: []Dependency{{NodeID: "target", DependsOn: "c"}, {NodeID: "target", DependsOn: "b"}, {NodeID: "b", DependsOn: "a"}, {NodeID: "c", DependsOn: "a"}},
		Artifacts:    []Artifact{{NodeID: "unrelated", ID: "other"}, {NodeID: "c", ID: "c-patch"}, {NodeID: "a", ID: "a-patch"}, {NodeID: "b", ID: "b-patch"}},
	}
	got, err := p.AcceptedArtifactIDs("target")
	if err != nil || !reflect.DeepEqual(got, []string{"a-patch", "b-patch", "c-patch"}) {
		t.Fatalf("accepted predecessor chain = %v, error = %v", got, err)
	}
	p.Nodes[0].State = NodeBlocked
	if _, err := p.AcceptedArtifactIDs("target"); err == nil {
		t.Fatal("blocked predecessor accepted as input")
	}
}

func TestCompletedPlanArtifactIsAtomicAndFenced(t *testing.T) {
	ctx, scheduler, p := schedulerPlan(t, []Node{{ID: "a", State: NodePending}, {ID: "b", State: NodePending}}, []Dependency{{NodeID: "b", DependsOn: "a"}}, SchedulerConfig{})
	claimAndStart(t, ctx, scheduler, p, "a")
	const artifact = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := scheduler.CompleteWithArtifact(ctx, p, "a", "lease-a", "complete-a", "complete-a", []EvidenceID{"verified-a"}, artifact); err != nil {
		t.Fatal(err)
	}
	loaded, err := scheduler.store.Load(ctx, p)
	if err != nil || len(loaded.Artifacts) != 1 || loaded.Artifacts[0] != (Artifact{NodeID: "a", ID: artifact}) || loaded.Nodes[0].State != NodeCompleted || loaded.Nodes[1].State != NodeReady {
		t.Fatalf("completed artifact = %+v, error = %v", loaded, err)
	}
	if err := scheduler.CompleteWithArtifact(ctx, p, "a", "lease-a", "stale", "stale", nil, artifact); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale artifact completion = %v", err)
	}
}

func TestNewGenerationKeepsOnlyPreservedCompletedArtifacts(t *testing.T) {
	const accepted = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const discarded = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	p := Plan{TenantID: "tenant", RepositoryID: "repo", TaskID: "task", ID: "plan", Generation: "one", State: PlanReplanning,
		Nodes:     []Node{{ID: "a", State: NodeCompleted}, {ID: "old", State: NodeBlocked}},
		Artifacts: []Artifact{{NodeID: "a", ID: accepted}, {NodeID: "old", ID: discarded}},
	}
	next, err := p.NewGeneration("two", []Node{{ID: "a", State: NodePending}, {ID: "new", State: NodePending}}, []Dependency{{NodeID: "new", DependsOn: "a"}})
	if err != nil || !reflect.DeepEqual(next.Artifacts, []Artifact{{NodeID: "a", ID: accepted}}) {
		t.Fatalf("successor artifacts = %+v, error = %v", next.Artifacts, err)
	}
	store := openTestSQLStore(t)
	if err := store.Create(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), next)
	if err != nil || !reflect.DeepEqual(loaded.Artifacts, next.Artifacts) {
		t.Fatalf("persisted successor artifacts = %+v, error = %v", loaded.Artifacts, err)
	}
}
