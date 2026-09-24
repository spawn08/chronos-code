package execution

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

var ErrTaskBudgetExceeded = errors.New("task budget exceeded")

type BudgetDimension string

const (
	BudgetRepairAttempts BudgetDimension = "repair_attempts"
	BudgetModelCalls     BudgetDimension = "model_calls"
	BudgetToolCalls      BudgetDimension = "tool_calls"
	BudgetWallTime       BudgetDimension = "wall_time"
	BudgetTokens         BudgetDimension = "tokens"
	BudgetCost           BudgetDimension = "cost"
)

type TaskLimits struct {
	RepairAttempts   int
	ModelCalls       int
	ToolCalls        int
	WallTime         time.Duration
	Tokens           int64
	CostMicrodollars int64
}

type BudgetSnapshot struct {
	Limits             TaskLimits
	RepairAttempts     int
	ModelCalls         int
	ToolCalls          int
	Tokens             int64
	CostMicrodollars   int64
	Elapsed            time.Duration
	ExhaustedDimension BudgetDimension
}

type BudgetExceededError struct {
	Dimension BudgetDimension
	Used      int64
	Limit     int64
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("%s: %s used %d of %d", ErrTaskBudgetExceeded, e.Dimension, e.Used, e.Limit)
}

func (e *BudgetExceededError) Unwrap() error { return ErrTaskBudgetExceeded }

type TaskBudget struct {
	mu      sync.Mutex
	limits  TaskLimits
	started time.Time
	used    BudgetSnapshot
}

func NewTaskBudget(limits TaskLimits, started time.Time) *TaskBudget {
	if started.IsZero() {
		started = time.Now()
	}
	return &TaskBudget{limits: limits, started: started}
}

func (b *TaskBudget) ConsumeRepairAttempt() error {
	return b.consume(BudgetRepairAttempts, 1)
}

func (b *TaskBudget) ConsumeModelCall() error {
	return b.consume(BudgetModelCalls, 1)
}

func (b *TaskBudget) ConsumeToolCall() error {
	return b.consume(BudgetToolCalls, 1)
}

func (b *TaskBudget) AddUsage(tokens, costMicrodollars int64) error {
	if tokens < 0 || costMicrodollars < 0 {
		return fmt.Errorf("task budget usage must be non-negative")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if tokens > math.MaxInt64-b.used.Tokens {
		return fmt.Errorf("task budget %s usage overflow", BudgetTokens)
	}
	if costMicrodollars > math.MaxInt64-b.used.CostMicrodollars {
		return fmt.Errorf("task budget %s usage overflow", BudgetCost)
	}
	b.used.Tokens += tokens
	b.used.CostMicrodollars += costMicrodollars
	if err := b.checkWallTimeLocked(time.Now()); err != nil {
		return err
	}
	if b.limits.Tokens > 0 && b.used.Tokens > b.limits.Tokens {
		b.used.ExhaustedDimension = BudgetTokens
		return &BudgetExceededError{Dimension: BudgetTokens, Used: b.used.Tokens, Limit: b.limits.Tokens}
	}
	if b.limits.CostMicrodollars > 0 && b.used.CostMicrodollars > b.limits.CostMicrodollars {
		b.used.ExhaustedDimension = BudgetCost
		return &BudgetExceededError{Dimension: BudgetCost, Used: b.used.CostMicrodollars, Limit: b.limits.CostMicrodollars}
	}
	return nil
}

func (b *TaskBudget) Check() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.checkWallTimeLocked(time.Now())
}

func (b *TaskBudget) Snapshot() BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	snapshot := b.used
	snapshot.Limits = b.limits
	snapshot.Elapsed = time.Since(b.started)
	return snapshot
}

func (b *TaskBudget) consume(dimension BudgetDimension, amount int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.checkWallTimeLocked(time.Now()); err != nil {
		return err
	}
	var used *int
	var limit int
	switch dimension {
	case BudgetRepairAttempts:
		used, limit = &b.used.RepairAttempts, b.limits.RepairAttempts
	case BudgetModelCalls:
		used, limit = &b.used.ModelCalls, b.limits.ModelCalls
	case BudgetToolCalls:
		used, limit = &b.used.ToolCalls, b.limits.ToolCalls
	default:
		return fmt.Errorf("unknown task budget dimension %q", dimension)
	}
	if err := checkLimit(dimension, int64(*used), amount, int64(limit)); err != nil {
		b.used.ExhaustedDimension = dimension
		return err
	}
	*used += int(amount)
	return nil
}

func (b *TaskBudget) checkWallTimeLocked(now time.Time) error {
	if b.limits.WallTime <= 0 {
		return nil
	}
	elapsed := now.Sub(b.started)
	if elapsed < b.limits.WallTime {
		return nil
	}
	b.used.ExhaustedDimension = BudgetWallTime
	return &BudgetExceededError{Dimension: BudgetWallTime, Used: int64(elapsed), Limit: int64(b.limits.WallTime)}
}

func checkLimit(dimension BudgetDimension, used, requested, limit int64) error {
	if limit > 0 && (used >= limit || requested > limit-used) {
		return &BudgetExceededError{Dimension: dimension, Used: used, Limit: limit}
	}
	return nil
}
