package execution

import (
	"errors"
	"math"
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

func TestTaskBudgetAddUsageRecordsOverage(t *testing.T) {
	tests := []struct {
		name          string
		limits        TaskLimits
		initialTokens int64
		initialCost   int64
		addedTokens   int64
		addedCost     int64
		wantDimension BudgetDimension
		wantUsed      int64
		wantTokens    int64
		wantCost      int64
	}{
		{
			name:          "tokens",
			limits:        TaskLimits{Tokens: 10, CostMicrodollars: 20},
			initialTokens: 8,
			initialCost:   15,
			addedTokens:   3,
			addedCost:     2,
			wantDimension: BudgetTokens,
			wantUsed:      11,
			wantTokens:    11,
			wantCost:      17,
		},
		{
			name:          "cost",
			limits:        TaskLimits{Tokens: 10, CostMicrodollars: 20},
			initialTokens: 5,
			initialCost:   19,
			addedTokens:   2,
			addedCost:     2,
			wantDimension: BudgetCost,
			wantUsed:      21,
			wantTokens:    7,
			wantCost:      21,
		},
		{
			name:          "tokens win when both exceed",
			limits:        TaskLimits{Tokens: 10, CostMicrodollars: 20},
			initialTokens: 8,
			initialCost:   15,
			addedTokens:   3,
			addedCost:     6,
			wantDimension: BudgetTokens,
			wantUsed:      11,
			wantTokens:    11,
			wantCost:      21,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			budget := NewTaskBudget(test.limits, time.Now())
			if err := budget.AddUsage(test.initialTokens, test.initialCost); err != nil {
				t.Fatal(err)
			}

			err := budget.AddUsage(test.addedTokens, test.addedCost)
			var exceeded *BudgetExceededError
			if !errors.As(err, &exceeded) {
				t.Fatalf("AddUsage() error = %v, want BudgetExceededError", err)
			}
			if exceeded.Dimension != test.wantDimension || exceeded.Used != test.wantUsed {
				t.Fatalf("AddUsage() error = %+v, want dimension %q used %d", exceeded, test.wantDimension, test.wantUsed)
			}
			snapshot := budget.Snapshot()
			if snapshot.Tokens != test.wantTokens || snapshot.CostMicrodollars != test.wantCost || snapshot.ExhaustedDimension != test.wantDimension {
				t.Fatalf("Snapshot() = %+v", snapshot)
			}
		})
	}
}

func TestTaskBudgetAddUsageOverflowDoesNotMutate(t *testing.T) {
	tests := []struct {
		name          string
		initialTokens int64
		initialCost   int64
		addedTokens   int64
		addedCost     int64
	}{
		{name: "tokens", initialTokens: math.MaxInt64, initialCost: 10, addedTokens: 1, addedCost: 1},
		{name: "cost", initialTokens: 10, initialCost: math.MaxInt64, addedTokens: 1, addedCost: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			budget := NewTaskBudget(TaskLimits{}, time.Now())
			if err := budget.AddUsage(test.initialTokens, test.initialCost); err != nil {
				t.Fatal(err)
			}
			before := budget.Snapshot()

			if err := budget.AddUsage(test.addedTokens, test.addedCost); err == nil {
				t.Fatal("AddUsage() error = nil, want overflow error")
			}
			after := budget.Snapshot()
			if after.Tokens != before.Tokens || after.CostMicrodollars != before.CostMicrodollars || after.ExhaustedDimension != before.ExhaustedDimension {
				t.Fatalf("Snapshot() after overflow = %+v, before = %+v", after, before)
			}
		})
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

func TestTaskBudgetRecordsUsageAfterWallTimeExpires(t *testing.T) {
	budget := NewTaskBudget(TaskLimits{WallTime: time.Second}, time.Now().Add(-2*time.Second))
	if err := budget.AddUsage(7, 11); !errors.Is(err, ErrTaskBudgetExceeded) {
		t.Fatalf("AddUsage() error = %v", err)
	}
	snapshot := budget.Snapshot()
	if snapshot.Tokens != 7 || snapshot.CostMicrodollars != 11 || snapshot.ExhaustedDimension != BudgetWallTime {
		t.Fatalf("Snapshot() = %+v", snapshot)
	}
}
