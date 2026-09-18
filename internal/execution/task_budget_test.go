package execution

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestTaskBudgetEnforcesEveryDimensionCumulatively(t *testing.T) {
	budget := NewTaskBudget(TaskLimits{RepairAttempts: 1, ModelCalls: 2, ToolCalls: 2, Tokens: 10, CostMicrodollars: 20}, time.Now())
	if err := budget.ConsumeRepairAttempt(); err != nil {
		t.Fatal(err)
	}
	if err := budget.ConsumeRepairAttempt(); !errors.Is(err, ErrTaskBudgetExceeded) {
		t.Fatalf("second repair error = %v", err)
	}
	if err := budget.ConsumeModelCall(); err != nil {
		t.Fatal(err)
	}
	if err := budget.ConsumeModelCall(); err != nil {
		t.Fatal(err)
	}
	if err := budget.ConsumeModelCall(); !errors.Is(err, ErrTaskBudgetExceeded) {
		t.Fatalf("third model call error = %v", err)
	}
	if err := budget.AddUsage(8, 15); err != nil {
		t.Fatal(err)
	}
	if err := budget.AddUsage(3, 0); !errors.Is(err, ErrTaskBudgetExceeded) {
		t.Fatalf("token overflow error = %v", err)
	}
}

func TestTaskBudgetConcurrentToolCallsDoNotExceedLimit(t *testing.T) {
	budget := NewTaskBudget(TaskLimits{ToolCalls: 8}, time.Now())
	var group sync.WaitGroup
	var allowed int
	var mu sync.Mutex
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			if budget.ConsumeToolCall() == nil {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	group.Wait()
	if allowed != 8 || budget.Snapshot().ToolCalls != 8 {
		t.Fatalf("allowed=%d snapshot=%+v", allowed, budget.Snapshot())
	}
}

func TestTaskBudgetWallTime(t *testing.T) {
	budget := NewTaskBudget(TaskLimits{WallTime: time.Second}, time.Now().Add(-2*time.Second))
	if err := budget.Check(); !errors.Is(err, ErrTaskBudgetExceeded) {
		t.Fatalf("Check() error = %v", err)
	}
}
