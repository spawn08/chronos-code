package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/execution"
)

func TestTaskRuntimeContextIdentity(t *testing.T) {
	first, err := newTaskRuntime("task-1", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := newTaskRuntime("task-2", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := withTaskRuntime(context.Background(), first)
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	child, ok := taskRuntimeFromContext(childCtx)
	if !ok || child != first {
		t.Fatal("child context did not inherit task runtime")
	}
	replaced, ok := taskRuntimeFromContext(withTaskRuntime(ctx, second))
	if !ok || replaced != second || replaced.taskID == first.taskID {
		t.Fatal("explicit child task did not replace runtime identity")
	}
}

func TestEvidenceRecorderNormalizesWritesAndCommands(t *testing.T) {
	runtime, err := newTaskRuntime("task-1", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	write, err := runtime.recordWrite("./internal/../main.go", "sha256", 12, execution.ProvenanceRuntime, now)
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	check, err := runtime.recordCommand("go test ./...", execution.CommandTest, &exitCode, execution.TerminalExited, []execution.Scope{{Kind: execution.ScopeDirectory, Path: "."}}, execution.ProvenanceRuntime, now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if write.Paths[0] != "main.go" || write.MutationRevision != 1 {
		t.Fatalf("write = %#v", write)
	}
	if !check.Passed || check.MutationRevision != 1 || check.Scopes[0].Kind != execution.ScopeDirectory {
		t.Fatalf("check = %#v", check)
	}
}

func TestEvidenceRecorderConcurrentIsolationAndSnapshots(t *testing.T) {
	first, err := newTaskRuntime("task-1", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := newTaskRuntime("task-2", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := range 32 {
		group.Add(2)
		go func() {
			defer group.Done()
			if _, recordErr := first.recordWrite(fmt.Sprintf("first-%d.go", i), "hash", 1, execution.ProvenanceRuntime, time.Now()); recordErr != nil {
				t.Errorf("first record: %v", recordErr)
			}
		}()
		go func() {
			defer group.Done()
			if _, recordErr := second.recordWrite(fmt.Sprintf("second-%d.go", i), "hash", 1, execution.ProvenanceRuntime, time.Now()); recordErr != nil {
				t.Errorf("second record: %v", recordErr)
			}
		}()
	}
	group.Wait()
	firstState, err := first.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	secondState, err := second.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(firstState.Events) != 32 || len(secondState.Events) != 32 || firstState.TaskID == secondState.TaskID {
		t.Fatalf("isolated states = (%d/%s, %d/%s)", len(firstState.Events), firstState.TaskID, len(secondState.Events), secondState.TaskID)
	}
	firstState.Events[0].Paths[0] = "mutated.go"
	fresh, err := first.snapshot()
	if err != nil || fresh.Events[0].Paths[0] == "mutated.go" {
		t.Fatalf("snapshot mutated ledger: %#v, %v", fresh.Events[0], err)
	}
}

func TestTaskRuntimeUnknownModelRequiresExplicitCostLimit(t *testing.T) {
	usage := model.Usage{PromptTokens: 10, CompletionTokens: 5}
	for _, test := range []struct {
		name      string
		costLimit int64
		wantErr   bool
	}{
		{name: "disabled", costLimit: 0},
		{name: "enabled", costLimit: 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, err := newTaskRuntime("task-1", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			runtime.budget = execution.NewTaskBudget(execution.TaskLimits{CostMicrodollars: test.costLimit}, time.Now())
			err = runtime.recordModelUsage("private-azure-deployment", usage)
			if test.wantErr && !errors.Is(err, budget.ErrUnknownModel) {
				t.Fatalf("recordModelUsage() error = %v, want ErrUnknownModel", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("recordModelUsage() error = %v, want unpriced model allowed without a cost limit", err)
			}
		})
	}
}
